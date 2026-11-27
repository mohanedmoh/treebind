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

// Package maintain packs the objects of the agent's Git directory, which nothing else does (docs/adr/0043). Every commit,
// fetch and resolution adds loose objects, each a file of its own that takes at least a block of the volume and an inode, and
// the agent runs neither gc nor maintenance: they detach from the call and pack the refs (docs/adr/0013). A repack does
// neither. It runs as a Git command of the Runner, in the loop of the agent, behind the fence of the writer lock and with a
// time limit, and it removes no object that is not in a pack that it has written, so that nothing that a ref, a reflog, the
// index or nothing at all reaches is lost: the commits that the refs of the deletion guard and of the resolutions park among them.
package maintain

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

const (
	// DefaultLooseLimit and DefaultPackLimit are the limits that Git has for gc --auto, gc.auto and gc.autoPackLimit: the loose
	// objects are packed when there are more than about 6700 of them, and every pack is put in one when there are 50 packs.
	DefaultLooseLimit = 6700
	DefaultPackLimit  = 50

	// DefaultTimeout is how long a repack may run. It holds up the loop of the agent while it runs, the pulls, the scans, the
	// commits and the pushes, and not the writer lock, whose heartbeat is apart.
	DefaultTimeout = 10 * time.Minute

	// DefaultReserve is the room on the volume that a repack leaves, beyond what it writes.
	DefaultReserve = 1 << 20

	// HoldAfterCutOff is how long no maintenance is tried after one that was cut off: an out-of-memory kill takes the agent
	// with its Git, and a repack that needs more memory than the sync container has would be killed again at each start.
	HoldAfterCutOff = 24 * time.Hour

	// markerName is the file in the state directory that says, while a repack runs, when it began. One that is there when no
	// repack runs was left by a kill.
	markerName = "maintaining"

	// windowMemory bounds the memory of the search for deltas of a repack, and threads makes it one search. Git's defaults are
	// no bound and a search for each processor, of up to ten blobs each, which is more than the memory limit of the sync
	// container (128 MiB) on a machine of many processors; a blob above core.bigFileThreshold, 16 MiB in the Runner, is not
	// searched at all.
	windowMemory = "32m"
	threads      = "1"

	// indexPerObject is a bound on what the index and the reverse index of a pack take for each object: 28 bytes and 4.
	indexPerObject = 64
	// fixedPerPack is a bound on what a pack and its indexes take besides their objects.
	fixedPerPack = 64 << 10
)

// Outcome says what a maintenance did.
type Outcome string

const (
	// NotDue means that there was nothing to pack: the loose objects and the packs are below their limits.
	NotDue Outcome = "NotDue"
	// Packed means that the loose objects that something reaches were put in a new pack, and removed.
	Packed Outcome = "Packed"
	// Consolidated means that every pack and every loose object was put in one pack.
	Consolidated Outcome = "Consolidated"
	// NoRoom means that nothing was written, because the volume has less room than the repack would write.
	NoRoom Outcome = "NoRoom"
	// Held means that nothing was written, because a maintenance was cut off less than HoldAfterCutOff ago.
	Held Outcome = "Held"
)

// Store is what the object store of the Git directory holds.
type Store struct {
	// Loose is how many loose objects there are, and Packs how many packs that are not kept with a .keep file.
	Loose, Packs int
	// Bytes is what the objects take on the volume: the blocks of the loose objects and of the files of the packs.
	Bytes int64
	// looseSize and packSize are the sizes of the files of the loose objects and of the packs.
	looseSize, packSize int64
}

// Result is what a maintenance did.
type Result struct {
	Outcome Outcome
	// Before and After are the object store before and after the repack. After is Before when nothing was written.
	Before, After Store
	// Need and Free are, for NoRoom, the room that the repack needed and the room that the volume had.
	Need, Free int64
	// Since is, for Held, when the maintenance that was cut off began.
	Since time.Time
	// Removed are the files that a Git that was stopped left among the objects, which were removed before the repack or
	// after it failed, relative to the Git directory.
	Removed []string
	// Remaining is true when the repack left the loose objects or the packs at their limit, as the loose objects that nothing
	// reaches are when there is no room to put every pack in one: a maintenance now would do nothing more.
	Remaining bool
}

// namedRemoved is how many of the files that were removed the note names.
const namedRemoved = 10

// String says what was done, for the end of the message of the check that the maintenance follows, and is empty when nothing
// was due.
func (r Result) String() string {
	var note string
	switch r.Outcome {
	case Packed:
		note = fmt.Sprintf("packed %s of the Git directory: its objects took %s and take %s",
			plural(max(r.Before.Loose-r.After.Loose, 0), "loose object"), mib(r.Before.Bytes), mib(r.After.Bytes))
		if r.Remaining {
			note += fmt.Sprintf("; %s that nothing reaches stay loose until the volume has the room to put every pack in one",
				plural(r.After.Loose, "loose object"))
		}
	case Consolidated:
		note = fmt.Sprintf("put %s and %s of the Git directory in one pack: its objects took %s and take %s",
			plural(r.Before.Packs, "pack"), plural(r.Before.Loose, "loose object"), mib(r.Before.Bytes), mib(r.After.Bytes))
	case NoRoom:
		note = fmt.Sprintf("the Git directory was not packed: it has %s and %s, and the repack needs %s free on the volume and the "+
			"volume has %s; give the claim more room", plural(r.Before.Loose, "loose object"), plural(r.Before.Packs, "pack"),
			mib(r.Need), mib(r.Free))
	case Held:
		note = fmt.Sprintf("the Git directory is not packed until %s: a maintenance that began at %s was cut off, as an out-of-memory "+
			"kill of the sync container does", r.Since.Add(HoldAfterCutOff).UTC().Format(time.RFC3339), r.Since.UTC().Format(time.RFC3339))
	}
	if len(r.Removed) > 0 {
		shown := strings.Join(r.Removed[:min(len(r.Removed), namedRemoved)], ", ")
		if more := len(r.Removed) - namedRemoved; more > 0 {
			shown += fmt.Sprintf(" and %d more", more)
		}
		removed := "removed what a Git command that was stopped left among the objects: " + shown
		if note == "" {
			return removed
		}
		note += "; " + removed
	}
	return note
}

// plural is a count and a noun, with an s when the count is not one.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// mib is a size in MiB, for people.
func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

// Options configure a Maintainer.
type Options struct {
	// Layout is where the Git directory and the state directory are.
	Layout layout.Layout
	// Git are the settings of the Runner. The directories and the look at the Git directory are taken from Layout.
	Git git.Options
	// Fence is called immediately before the first write, and nothing is written when it returns an error. It is the check that
	// the agent still holds the writer lock (docs/adr/0023). With none, nothing is checked.
	Fence func(ctx context.Context) error
	// Timeout is how long a repack may run. DefaultTimeout when it is zero.
	Timeout time.Duration
	// LooseLimit and PackLimit are the number of loose objects and of packs from which a maintenance packs. The defaults
	// above when they are zero.
	LooseLimit, PackLimit int
	// Reserve is the room that a repack leaves on the volume. DefaultReserve when it is zero.
	Reserve int64
	// Free says how many bytes the volume of a directory has free for the agent. The file system's when nil.
	Free func(dir string) (int64, error)
	// Now is the time of the agent. time.Now when nil.
	Now func() time.Time
}

// Maintainer packs the objects of one Git directory. It is not safe for concurrent use: the agent maintains in the same loop as
// its pulls, its scans, its commits and its pushes, and never beside one of them.
type Maintainer struct {
	o   Options
	git *git.Runner
}

// New makes a Maintainer.
func New(o Options) (*Maintainer, error) {
	if o.Timeout < 0 || o.LooseLimit < 0 || o.PackLimit < 0 || o.Reserve < 0 {
		return nil, errors.New("maintain: the timeout, the limits and the reserve must not be negative")
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultTimeout
	}
	if o.LooseLimit == 0 {
		o.LooseLimit = DefaultLooseLimit
	}
	if o.PackLimit == 0 {
		o.PackLimit = DefaultPackLimit
	}
	if o.Reserve == 0 {
		o.Reserve = DefaultReserve
	}
	if o.Free == nil {
		o.Free = freeSpace
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	runner, err := git.New(o.Layout.RunnerOptions(o.Git))
	if err != nil {
		return nil, fmt.Errorf("maintain: %w", err)
	}
	return &Maintainer{o: o, git: runner}, nil
}

// freeSpace is the room that the file system of the directory has for a user who is not root.
func freeSpace(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("the free room of the volume of %s cannot be read: %w", dir, err)
	}
	// Bsize is an int64 on linux/amd64 and a narrower or unsigned integer on other systems, so it is converted.
	return int64(st.Bavail) * int64(st.Bsize), nil //nolint:gosec,unconvert // the free blocks of a volume fit in an int64
}

// looseName is the name of a loose object in its directory: the rest of the id after its first two digits, of SHA-1 or SHA-256.
var looseName = regexp.MustCompile(`^[0-9a-f]{38}(?:[0-9a-f]{24})?$`)

// packIndex is the name of the index of a pack, with the name of the pack.
var packIndex = regexp.MustCompile(`^(pack-[0-9a-f]{40}(?:[0-9a-f]{24})?)\.idx$`)

// sampleDir is the directory of loose objects that Git looks at to guess how many there are: objects/17.
const sampleDir = "objects/17"

// Due says whether a maintenance would pack anything, from a look at two directories, as Git's gc --auto looks: the loose objects
// in objects/17, of which there are a 256th of all of them on average, and the packs that are not kept. It writes nothing.
func (m *Maintainer) Due() (bool, error) {
	root, err := os.OpenRoot(m.o.Layout.GitDir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	loose := 0
	entries, err := fs.ReadDir(root.FS(), sampleDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && looseName.MatchString(entry.Name()) {
			loose++
		}
	}
	if loose > (m.o.LooseLimit+255)/256 {
		return true, nil
	}
	packs, err := countPacks(root)
	if err != nil {
		return false, err
	}
	return packs >= m.o.PackLimit, nil
}

// countPacks counts the packs that are not kept.
func countPacks(root *os.Root) (int, error) {
	entries, err := fs.ReadDir(root.FS(), path.Join("objects", "pack"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	packs := 0
	for _, entry := range entries {
		if m := packIndex.FindStringSubmatch(entry.Name()); m != nil && entry.Type().IsRegular() && !names[m[1]+".keep"] {
			packs++
		}
	}
	return packs, nil
}

// survey looks at every directory of the object store: what the loose objects and the packs are, and what they take.
func survey(gitDir string) (Store, error) {
	var s Store
	root, err := os.OpenRoot(gitDir)
	if err != nil {
		return s, err
	}
	defer func() { _ = root.Close() }()
	dirs, err := fs.ReadDir(root.FS(), "objects")
	if err != nil {
		return s, err
	}
	for _, dir := range dirs {
		if !dir.IsDir() || len(dir.Name()) != 2 || strings.Trim(dir.Name(), "0123456789abcdef") != "" {
			continue
		}
		entries, err := fs.ReadDir(root.FS(), path.Join("objects", dir.Name()))
		if err != nil {
			return s, err
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !looseName.MatchString(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return s, err
			}
			s.Loose++
			s.looseSize += info.Size()
			s.Bytes += blocks(info)
		}
	}
	entries, err := fs.ReadDir(root.FS(), path.Join("objects", "pack"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "pack-") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return s, err
		}
		s.packSize += info.Size()
		s.Bytes += blocks(info)
	}
	if s.Packs, err = countPacks(root); err != nil {
		return s, err
	}
	return s, nil
}

// blocks is what a file takes on the volume: its blocks where the system says, and its size where it does not.
func blocks(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return info.Size()
}

// Maintain packs the objects of the Git directory when there are enough to pack, and says what it did. With as many packs as
// PackLimit it puts every pack and every loose object in one pack, unreachable objects too; with as many loose objects as
// LooseLimit it puts the loose objects that something reaches in a new pack. Either removes what is in the new pack, and nothing
// else, so that no object is lost. It checks first that the volume has the room: the new pack is written beside what it holds,
// which is removed after it. Without the room for every pack it packs the loose objects, and without the room for those nothing.
//
// The files that a Git that was stopped left among the objects are removed first, and again after a repack that failed, which
// is how a repack that is killed in the middle is put right (docs/adr/0028). A repack that was cut off with the agent, as an
// out-of-memory kill does, holds every maintenance for HoldAfterCutOff.
func (m *Maintainer) Maintain(ctx context.Context) (Result, error) {
	gitDir := m.o.Layout.GitDir
	since, held, expired, err := m.cutOff()
	if err != nil {
		return Result{}, err
	}
	if held {
		return m.held(ctx, since)
	}
	before, err := survey(gitDir)
	if err != nil {
		return Result{}, fmt.Errorf("the objects of the Git directory cannot be looked at: %w", err)
	}
	res := Result{Before: before, After: before}
	full, loose := before.Packs >= m.o.PackLimit, before.Loose >= m.o.LooseLimit
	if !full && !loose {
		res.Outcome = NotDue
		return res, nil
	}
	if res.Free, err = m.o.Free(gitDir); err != nil {
		return res, err
	}
	needLoose, needFull := m.need(before)
	switch {
	case full && res.Free >= needFull:
	case loose && res.Free >= needLoose:
		full = false
	default:
		res.Outcome, res.Need = NoRoom, needLoose
		if !loose {
			res.Need = needFull
		}
		return res, nil
	}
	if err := m.fence(ctx); err != nil {
		return res, err
	}
	if expired {
		if err := m.removeMarker(); err != nil {
			return res, err
		}
	}
	if res.Removed, err = git.RemoveStaleTempObjects(gitDir, 0); err != nil {
		return res, fmt.Errorf("the Git directory was not packed: what a Git command that was stopped left among the objects "+
			"cannot be removed: %w", err)
	}
	if err := m.mark(); err != nil {
		return res, err
	}
	res, full, err = m.repackAll(ctx, res, full)
	unmarked := m.unmarkAfter(ctx, err)
	if err != nil {
		res.Removed = append(res.Removed, m.afterFailure(ctx)...)
		return res, fmt.Errorf("the Git directory could not be packed: %w", err)
	}
	res.Outcome = Packed
	if full {
		res.Outcome = Consolidated
	}
	res.Remaining = res.After.Loose >= m.o.LooseLimit || res.After.Packs >= m.o.PackLimit
	return res, unmarked
}

// need is the room that a repack of the loose objects needs, and the room that a repack of every pack into one needs.
func (m *Maintainer) need(s Store) (loose, full int64) {
	loose = s.looseSize + int64(s.Loose)*indexPerObject + fixedPerPack + m.o.Reserve
	return loose, loose + s.packSize
}

// repackAll runs the repack, and then looks at what it left. A repack of the loose objects packs only those that something
// reaches, and when the loose objects that nothing reaches are still at the limit they are put in one pack with every other
// object, when the volume has the room and the fence passes. Otherwise a maintenance at the next pull would do the same again.
func (m *Maintainer) repackAll(ctx context.Context, res Result, full bool) (Result, bool, error) {
	gitDir := m.o.Layout.GitDir
	if _, err := m.repack(ctx, full); err != nil {
		return res, full, err
	}
	after, err := survey(gitDir)
	if err != nil {
		return res, full, fmt.Errorf("the objects of the Git directory cannot be looked at after the repack: %w", err)
	}
	res.After = after
	if full || after.Loose < m.o.LooseLimit {
		return res, full, nil
	}
	// The volume is asked again, since the first repack has freed the blocks of the loose objects that it packed.
	_, needFull := m.need(after)
	if free, err := m.o.Free(gitDir); err != nil || free < needFull {
		return res, full, nil
	}
	if err := m.fence(ctx); err != nil {
		return res, full, err
	}
	if _, err := m.repack(ctx, true); err != nil {
		return res, full, err
	}
	if res.After, err = survey(gitDir); err != nil {
		return res, true, fmt.Errorf("the objects of the Git directory cannot be looked at after the repack: %w", err)
	}
	return res, true, nil
}

// held is a maintenance that is held. What the repack that was cut off left among the objects can keep the volume full, and the
// start removes only what has not changed for longer than a Git command of an agent may run, so it is removed here, behind the
// fence, and only when there is something to remove.
func (m *Maintainer) held(ctx context.Context, since time.Time) (Result, error) {
	res := Result{Outcome: Held, Since: since}
	left, err := git.StaleTempObjects(m.o.Layout.GitDir, 0)
	if err != nil || len(left) == 0 {
		return res, err
	}
	if err := m.fence(ctx); err != nil {
		return res, err
	}
	res.Removed, err = git.RemoveStaleTempObjects(m.o.Layout.GitDir, 0)
	return res, err
}

// killedByAnother reports whether a repack that failed, or the pack-objects that it started, was killed with SIGKILL by
// something other than the agent, as the out-of-memory killer of the container does. The agent kills a command only when its
// context ended, which the Runner says as a cancellation or a timeout, and not as the exit of the command.
func killedByAnother(err error) bool {
	failure, ok := errors.AsType[*git.Error](err)
	if !ok {
		return false
	}
	if exit, ok := errors.AsType[*exec.ExitError](failure.Cause); ok {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() && status.Signal() == syscall.SIGKILL {
			return true
		}
	}
	return strings.Contains(failure.Stderr, "died of signal 9")
}

// repack runs the repack: of every pack and every loose object into one pack, with the objects that nothing reaches, when full is
// true, and of the loose objects that something reaches into a new pack when it is not. -n leaves out update-server-info, which
// would write info/refs, which the check of the Git directory refuses (docs/adr/0019).
func (m *Maintainer) repack(ctx context.Context, full bool) (git.Result, error) {
	args := []string{"repack", "-d", "-n", "-q", "--threads=" + threads, "--window-memory=" + windowMemory}
	if full {
		args = append(args, "-a", "--keep-unreachable")
	}
	return m.git.RunTimeout(ctx, m.o.Timeout, args...)
}

// fence checks the writer lock.
func (m *Maintainer) fence(ctx context.Context) error {
	if m.o.Fence == nil {
		return nil
	}
	if err := m.o.Fence(ctx); err != nil {
		return fmt.Errorf("the Git directory was not packed, because the writer lock could not be checked: %w", err)
	}
	return nil
}

// afterFailure removes what a repack that failed left among the objects, behind the fence. Nothing is removed when the agent was
// told to stop, or lost its lock, which stops the repack too: another agent may write there now, and the next start removes it.
func (m *Maintainer) afterFailure(ctx context.Context) []string {
	if ctx.Err() != nil || m.fence(ctx) != nil {
		return nil
	}
	removed, _ := git.RemoveStaleTempObjects(m.o.Layout.GitDir, 0)
	return removed
}

// cutOff says whether a maintenance was cut off less than HoldAfterCutOff ago, and when it began, and whether a marker is there
// that is older, or cannot be read as a time, which is one that was cut off as it was written, before its repack began. Such a
// marker is removed by the maintenance that follows, behind the fence.
func (m *Maintainer) cutOff() (since time.Time, held, expired bool, err error) {
	root, err := os.OpenRoot(m.o.Layout.StateDir)
	if err != nil {
		return time.Time{}, false, false, fmt.Errorf("the state directory cannot be opened: %w", err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(markerName)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, false, nil
	}
	if err != nil {
		return time.Time{}, false, false, fmt.Errorf("%s cannot be read: %w", filepath.Join(m.o.Layout.StateDir, markerName), err)
	}
	unix, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	since = time.Unix(unix, 0)
	if err == nil && m.o.Now().Sub(since) < HoldAfterCutOff {
		return since, true, false, nil
	}
	return time.Time{}, false, true, nil
}

// mark writes the marker, the time of the agent in seconds. It is not synced: a crash of the node that loses it loses one hold.
func (m *Maintainer) mark() error {
	root, err := os.OpenRoot(m.o.Layout.StateDir)
	if err != nil {
		return fmt.Errorf("the state directory cannot be opened: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(markerName, []byte(strconv.FormatInt(m.o.Now().Unix(), 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("the Git directory was not packed, because %s cannot be written: %w",
			filepath.Join(m.o.Layout.StateDir, markerName), err)
	}
	return nil
}

// unmarkAfter removes the marker after a repack, unless the repack was killed by something other than the agent, which the
// marker then holds for a day, or the fence fails: an agent that has lost its lock writes nothing more. The fence is checked
// without the context of the loop, which a stop of the agent cancels while it still holds the lock.
func (m *Maintainer) unmarkAfter(ctx context.Context, failed error) error {
	if killedByAnother(failed) {
		return nil
	}
	if err := m.fence(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	if err := m.removeMarker(); err != nil {
		return fmt.Errorf("%w, and no maintenance is tried for a day", err)
	}
	return nil
}

// removeMarker removes the marker. The caller has checked the fence.
func (m *Maintainer) removeMarker() error {
	root, err := os.OpenRoot(m.o.Layout.StateDir)
	if err != nil {
		return fmt.Errorf("the state directory cannot be opened: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(markerName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s cannot be removed: %w", filepath.Join(m.o.Layout.StateDir, markerName), err)
	}
	return nil
}
