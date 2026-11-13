/*
Copyright The Treebind Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The contention tests run on the system clock, because separate processes
// share no other. Their TTL is far longer than a holder stays in its critical
// section, so that a slow machine does not look like a holder that froze. They
// run side by side, because most of their time is spent waiting for a TTL.
const (
	contentionTTL      = time.Second
	contentionInterval = 5 * time.Millisecond
)

// The environment of a helper process. TestMain turns the test binary into an
// agent when helperMode is set.
const (
	helperMode    = "TREEBIND_LOCK_TEST_MODE"
	helperDir     = "TREEBIND_LOCK_TEST_DIR"
	helperWitness = "TREEBIND_LOCK_TEST_WITNESS"
	helperName    = "TREEBIND_LOCK_TEST_NAME"
	helperRounds  = "TREEBIND_LOCK_TEST_ROUNDS"
	helperAbandon = "TREEBIND_LOCK_TEST_ABANDON"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperMode); mode != "" {
		if err := runHelper(mode); err != nil {
			fmt.Println("FAILED:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func contentionConfig(dir, holder string) Config {
	return Config{Dir: dir, Holder: holder, TTL: contentionTTL, Interval: contentionInterval}
}

// TestContendersInGoroutines lets agents fight for one lock over many rounds.
// Most hand the lock over cleanly, and two walk away without a word, the way a
// frozen pod does. No two may ever be inside at once.
func TestContendersInGoroutines(t *testing.T) {
	t.Parallel()
	const contenders, rounds = 8, 12
	dir := lockDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var (
		inside    atomic.Int32
		mu        sync.Mutex
		epochs    []uint64
		abandoned []*Lock
		failures  []string
	)
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, fmt.Sprintf(format, args...))
	}

	var group sync.WaitGroup
	for id := range contenders {
		group.Go(func() {
			for round := range rounds {
				l, err := Acquire(ctx, contentionConfig(dir, "agent-"+strconv.Itoa(id)))
				if err != nil {
					fail("agent %d, round %d: Acquire: %v", id, round, err)
					return
				}
				if n := inside.Add(1); n != 1 {
					fail("agent %d, round %d: %d holders at once", id, round, n)
				}
				if err := l.Check(); err != nil {
					fail("agent %d, round %d: Check of a lock just taken: %v", id, round, err)
				}
				mu.Lock()
				epochs = append(epochs, l.Epoch())
				mu.Unlock()
				runtime.Gosched()
				inside.Add(-1)

				if id < 2 && round == 0 {
					mu.Lock()
					abandoned = append(abandoned, l)
					mu.Unlock()
					continue
				}
				if err := l.Release(); err != nil {
					fail("agent %d, round %d: Release: %v", id, round, err)
				}
			}
		})
	}
	group.Wait()

	for _, failure := range failures {
		t.Error(failure)
	}
	if len(epochs) != contenders*rounds {
		t.Fatalf("%d tenures happened, want %d", len(epochs), contenders*rounds)
	}
	for i := 1; i < len(epochs); i++ {
		if epochs[i] <= epochs[i-1] {
			t.Fatalf("tenure %d has epoch %d after epoch %d", i, epochs[i], epochs[i-1])
		}
	}
	for _, l := range abandoned {
		if err := l.Check(); !errors.Is(err, ErrLost) {
			t.Errorf("the abandoned lock of epoch %d answers %v, want ErrLost", l.Epoch(), err)
		}
	}
}

// TestContendersInProcesses is the same fight between separate processes, which
// share nothing but the directory. Inside, each one reads a counter and writes it
// back one higher, so a second holder would also show as a lost count.
func TestContendersInProcesses(t *testing.T) {
	t.Parallel()
	const processes, rounds = 4, 10
	dir, witness := lockDir(t), t.TempDir()

	agents := make([]*helper, processes)
	for id := range agents {
		agents[id] = startHelper(t, "contend", dir, "agent-"+strconv.Itoa(id),
			helperWitness+"="+witness,
			helperRounds+"="+strconv.Itoa(rounds),
			helperAbandon+"="+strconv.FormatBool(id < 2))
	}
	for id, agent := range agents {
		if line := agent.line(t); line != "done" {
			t.Errorf("agent %d said %q, want done", id, line)
		}
		agent.exit(t)
	}

	if got := readNumber(t, filepath.Join(witness, "count")); got != processes*rounds {
		t.Errorf("the counter is %d after %d tenures", got, processes*rounds)
	}
}

// TestAFrozenProcessIsTakenOver stops a holder with SIGSTOP, as a frozen node
// would, lets another process take over and wakes the holder up again. Its
// heartbeat has to report the loss.
func TestAFrozenProcessIsTakenOver(t *testing.T) {
	t.Parallel()
	dir := lockDir(t)

	frozen := startHelper(t, "hold", dir, "frozen")
	held := strings.Fields(frozen.line(t))
	if len(held) != 2 || held[0] != "held" {
		t.Fatalf("the holder said %q", held)
	}
	if err := frozen.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}

	next := startHelper(t, "hold", dir, "next")
	took := strings.Fields(next.line(t))
	if len(took) != 2 || took[0] != "held" {
		t.Fatalf("the contender said %q", took)
	}
	if before, after := number(t, held[1]), number(t, took[1]); after <= before {
		t.Errorf("the new epoch %d is not above the old one %d", after, before)
	}

	if err := frozen.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if line := frozen.line(t); line != "lost" {
		t.Errorf("the holder that woke up said %q, want lost", line)
	}

	// The new holder is still the holder. Closing its input lets it release.
	if err := next.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if line := next.line(t); line != "released" {
		t.Errorf("the new holder said %q, want released", line)
	}
	frozen.exit(t)
	next.exit(t)
}

// helper is a test binary that runs as an agent in its own process.
type helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
}

func startHelper(t *testing.T, mode, dir, name string, env ...string) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperMode+"="+mode, helperDir+"="+dir, helperName+"="+name)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// A helper that is still there has failed its test. It may be stopped,
		// and a stopped process only dies of a kill.
		_ = cmd.Process.Kill()
	})

	h := &helper{cmd: cmd, stdin: stdin, lines: make(chan string, 16)}
	go func() {
		defer close(h.lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			h.lines <- scanner.Text()
		}
	}()
	return h
}

// line waits for the next line the helper prints.
func (h *helper) line(t *testing.T) string {
	t.Helper()
	select {
	case line, ok := <-h.lines:
		if !ok {
			t.Fatal("the helper process ended without the line the test waits for")
		}
		return line
	case <-time.After(2 * time.Minute):
		t.Fatal("the helper process said nothing")
		return ""
	}
}

// exit waits for the helper to end and fails the test unless it ended well.
func (h *helper) exit(t *testing.T) {
	t.Helper()
	for range h.lines {
	}
	if err := h.cmd.Wait(); err != nil {
		t.Errorf("the helper process failed: %v", err)
	}
}

func number(t *testing.T, text string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func readNumber(t *testing.T, path string) uint64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return number(t, string(data))
}

// runHelper is what the test binary does as an agent.
func runHelper(mode string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg := contentionConfig(os.Getenv(helperDir), os.Getenv(helperName))
	switch mode {
	case "contend":
		return contend(ctx, cfg)
	case "hold":
		return hold(ctx, cfg)
	}
	return fmt.Errorf("unknown mode %q", mode)
}

// contend takes the lock for a number of rounds. While it holds the lock it puts
// a witness file down that only one process can make, and it moves a counter and
// the last epoch on, neither of which survives two holders.
func contend(ctx context.Context, cfg Config) error {
	witness := os.Getenv(helperWitness)
	rounds, err := strconv.Atoi(os.Getenv(helperRounds))
	if err != nil {
		return err
	}
	for round := range rounds {
		l, err := Acquire(ctx, cfg)
		if err != nil {
			return fmt.Errorf("round %d: Acquire: %w", round, err)
		}
		inside := filepath.Join(witness, "inside")
		file, err := os.OpenFile(inside, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("round %d, epoch %d: two holders at once: %w", round, l.Epoch(), err)
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := l.Check(); err != nil {
			return fmt.Errorf("round %d: Check of a lock just taken: %w", round, err)
		}
		if err := moveOn(filepath.Join(witness, "epoch"), l.Epoch()); err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		if err := moveOn(filepath.Join(witness, "count"), 0); err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		if err := os.Remove(inside); err != nil {
			return err
		}
		if round == 0 && os.Getenv(helperAbandon) == "true" {
			continue
		}
		if err := l.Release(); err != nil {
			return fmt.Errorf("round %d: Release: %w", round, err)
		}
	}
	fmt.Println("done")
	return nil
}

// moveOn reads the number in the file and writes a higher one back: to, which
// has to be higher, or the next number when to is zero. The file is written in
// place, so only the lock keeps two processes from spoiling it.
func moveOn(path string, to uint64) error {
	var last uint64
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if last, err = strconv.ParseUint(string(data), 10, 64); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if to == 0 {
		to = last + 1
	}
	if to <= last {
		return fmt.Errorf("%s would go from %d to %d", filepath.Base(path), last, to)
	}
	return os.WriteFile(path, []byte(strconv.FormatUint(to, 10)), 0o644)
}

// hold takes the lock, keeps it with a heartbeat and says when it lost it. When
// its input closes first, it releases the lock.
func hold(ctx context.Context, cfg Config) error {
	l, err := Acquire(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Println("held", l.Epoch())

	kept := make(chan error, 1)
	go func() { kept <- l.Keep(ctx) }()
	closed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(closed)
	}()

	select {
	case err := <-kept:
		if !errors.Is(err, ErrLost) {
			return fmt.Errorf("Keep returned %v, want ErrLost", err)
		}
		if err := l.Check(); !errors.Is(err, ErrLost) {
			return fmt.Errorf("Check after the loss returned %v, want ErrLost", err)
		}
		fmt.Println("lost")
	case <-closed:
		if err := l.Release(); err != nil {
			return err
		}
		fmt.Println("released")
	}
	return nil
}
