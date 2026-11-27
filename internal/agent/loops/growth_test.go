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

package loops

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/maintain"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
)

// The growth of the Git directory is measured, and not asserted, by TestGrowthOfTheGitDirectory: it runs only when
// TREEBIND_GROWTH_MINUTES says how many minutes of sync to run, which takes far longer than a test should (#289).
//
// Each minute is what the defaults allow an application that saves files every few seconds: the breaker lets one commit
// through in a minute (spec.commit.maxPerHour, 60), and the pull comes every 30 seconds (spec.sync.interval). On the
// other side another Treebind on the same repository commits and pushes every other minute, so that both sides change
// all the time and some pushes are refused and resolved. TREEBIND_GROWTH_COMMITS_PER_MINUTE raises the commits of the
// application, for a Treebind whose limit is raised. TREEBIND_GROWTH_MAINTAIN=1 runs the maintenance of the agent after each
// pull of A, when it is due, as the agent does (docs/adr/0043).

const (
	gitRepack   = "repack"
	expireNow   = "--expire=now"
	growthFiles = 200
	growthDirs  = 10
	growthLines = 120
)

// dashboard is a file of the kind that an application keeps: JSON of about 6 KB, of which a save changes a few values.
func dashboard(values []int) string {
	var b strings.Builder
	b.WriteString("{\n  \"panels\": {\n")
	for i, v := range values {
		fmt.Fprintf(&b, "    \"panel-%03d\": {\"title\": \"Requests by status, panel %03d\", \"value\": %d},\n", i, i, v)
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

func growthName(i int) string { return fmt.Sprintf("dir-%02d/dashboard-%03d.json", i%growthDirs, i) }

// sizes are what the Git directory takes on the volume.
type sizes struct {
	files, loose, packs       int
	apparent, allocated, logs int64
	looseBytes, packBytes     int64
}

func measure(t *testing.T, gitDir string) sizes {
	t.Helper()
	var s sizes
	err := filepath.WalkDir(gitDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		s.allocated += st.Blocks * 512
		if d.IsDir() {
			return nil
		}
		s.files++
		s.apparent += info.Size()
		rel, _ := filepath.Rel(gitDir, path)
		switch {
		case strings.HasPrefix(rel, "logs/"):
			s.logs += st.Blocks * 512
		case strings.HasPrefix(rel, "objects/pack/"):
			s.packs++
			s.packBytes += st.Blocks * 512
		case strings.HasPrefix(rel, "objects/") && len(filepath.Base(filepath.Dir(rel))) == 2:
			s.loose++
			s.looseBytes += st.Blocks * 512
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (s sizes) String() string {
	mb := func(n int64) string { return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20)) }
	return fmt.Sprintf("on disk %s (apparent %s) in %d files; loose objects %d (%s); pack files %d (%s); reflogs %s",
		mb(s.allocated), mb(s.apparent), s.files, s.loose, mb(s.looseBytes), s.packs, mb(s.packBytes), mb(s.logs))
}

func envInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q: %v", name, raw, err)
	}
	return n
}

func TestGrowthOfTheGitDirectory(t *testing.T) {
	minutes := envInt(t, "TREEBIND_GROWTH_MINUTES", 0)
	if minutes <= 0 {
		t.Skip("a measurement: set TREEBIND_GROWTH_MINUTES to the minutes of sync to run")
	}
	perMinute := envInt(t, "TREEBIND_GROWTH_COMMITS_PER_MINUTE", 1)
	every := envInt(t, "TREEBIND_GROWTH_REPORT_MINUTES", 360)

	f := gittest.New(t)
	rng := rand.New(rand.NewPCG(289, 1))
	values := make([][]int, growthFiles)
	first := map[string]string{}
	for i := range values {
		values[i] = make([]int, growthLines)
		for j := range values[i] {
			values[i][j] = rng.IntN(100000)
		}
		first[growthName(i)] = dashboard(values[i])
	}
	f.Publish(branchMain, first)
	a := newNode(t, f, "A", subPath, pull.FollowRemote)
	b := newNode(t, f, "B", subPath, pull.FollowRemote)
	// B's application keeps to the last fifty files and A's to the others, as two teams would: a conflict is then two
	// sides that each have commits, and the files that both change are rare.
	save := func(n *node, lo, hi, count int) {
		for range count {
			i := lo + rng.IntN(hi-lo)
			for range 3 {
				values[i][rng.IntN(growthLines)] = rng.IntN(100000)
			}
			n.write(growthName(i), dashboard(values[i]))
		}
	}
	var maintainer *maintain.Maintainer
	if envInt(t, "TREEBIND_GROWTH_MAINTAIN", 0) == 1 {
		var err error
		if maintainer, err = maintain.New(maintain.Options{Layout: a.l, Git: f.GitOptions()}); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("start: %s", measure(t, a.l.GitDir))
	for minute := 1; minute <= minutes; minute++ {
		b.pull()
		if minute%2 == 0 {
			save(b, growthFiles-50, growthFiles, 1)
			b.commit()
			b.push()
		}
		for range perMinute {
			save(a, 0, growthFiles-50, 2)
			a.commit()
			a.push()
		}
		for range 2 {
			a.pull()
			if maintainer != nil {
				maintainOnce(t, maintainer, minute)
			}
		}
		if minute%every == 0 || minute == minutes {
			t.Logf("minute %d (%.2f days): A made %d commits, %d resolutions, %d updates: %s",
				minute, float64(minute)/1440, a.commits, a.resolved, a.advanced, measure(t, a.l.GitDir))
		}
		if minute%1440 == 0 || minute == minutes {
			reclaim(t, a)
		}
	}
}

// maintainOnce runs the maintenance when it is due, and says what it did.
func maintainOnce(t *testing.T, m *maintain.Maintainer, minute int) {
	t.Helper()
	due, err := m.Due()
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		return
	}
	started := time.Now()
	got, err := m.Maintain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("minute %d: maintenance %s in %s: %s", minute, got.Outcome, time.Since(started).Round(time.Millisecond), got)
}

// reclaim says what maintenance would take back, on copies of the Git directory.
func reclaim(t *testing.T, a *node) {
	t.Helper()
	try := func(name string, commands ...[]string) {
		dir := filepath.Join(gittest.TempDir(t), "git")
		copyDir(t, a.l.GitDir, dir)
		var rss int64
		for _, args := range commands {
			cmd := exec.Command(gittest.Binary(t), args...)
			cmd.Env = []string{"GIT_DIR=" + dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "HOME=/nonexistent"}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v: %s", name, err, out)
			}
			// The largest resident set of the command and of the commands that it started, in KiB.
			if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
				rss = max(rss, usage.Maxrss)
			}
		}
		t.Logf("after %s (largest resident set %d MiB): %s", name, rss>>10, measure(t, dir))
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	try("repack -d (the loose objects packed)", []string{gitRepack, "-d", "-n", "-q"})
	try("the repack of the agent of every pack into one", []string{gitRepack, "-d", "-n", "-q", "--threads=1", "--window-memory=32m",
		"-a", "--keep-unreachable"})
	try("repack -a -d (one pack)", []string{gitRepack, "-a", "-d", "-n", "-q"})
	try("repack -a -d and prune of what no ref, reflog or index reaches", []string{gitRepack, "-a", "-d", "-n", "-q"},
		[]string{"prune", expireNow})
	try("reflogs expired, repack -a -d and prune", []string{"reflog", "expire", expireNow, "--expire-unreachable=now", "--all"},
		[]string{gitRepack, "-a", "-d", "-n", "-q"}, []string{"prune", expireNow})
}

// copyDir copies a directory of regular files.
func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
