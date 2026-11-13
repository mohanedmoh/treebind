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

// Package git is the only way the agent starts the git binary.
//
// The volume that Git works on is shared with an application that can write
// there, and a Git command runs programs and reads files on the strength of
// what the repository, the user and the system configure: hooks, filters, a
// credential helper, an ssh command, a file:// or ext:: URL. The Runner takes all
// of that out of the hands of any file an application could write. Every command
// gets the same configuration on its command line (-c), an environment built from
// nothing, a fixed Git dir and work tree, a timeout, and a bounded output, and
// only the subcommands the agent needs can be asked for (docs/adr/0013).
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// DefaultTimeout is how long a command may run when the caller does not say.
	// A fetch of a large repository needs a longer one, given per call.
	DefaultTimeout = 2 * time.Minute

	// DefaultMaxOutput is how much a command may write to its standard output.
	DefaultMaxOutput = 16 << 20

	// stderrLimit is how much of the end of the standard error is kept. The
	// reason a command failed is its last lines.
	stderrLimit = 64 << 10

	// killGrace is how long Wait waits for the output of a killed command to
	// close. A process that left the group, which an ssh with a control master
	// can, holds the pipe open, and would otherwise hold the call.
	killGrace = 2 * time.Second

	// defaultPath is where the programs that Git starts itself are looked for.
	defaultPath = "/usr/local/bin:/usr/bin:/bin"
)

// Options configure a Runner.
type Options struct {
	// GitDir is the repository's Git directory, and WorkTree is the directory
	// that holds its files. Both are absolute and clean, and are passed to
	// every command. Where the Git directory lives is decided in docs/adr/0016: beside
	// the synced directory, so that an application that mounts only the sub path cannot
	// reach it. Outside what the application can write is safer, because nothing it
	// writes can then be read as configuration.
	GitDir, WorkTree string

	// Git is the git binary. The first git on PATH is used when it is empty.
	Git string

	// AllowGitDirInWorkTree lets the Git directory be inside the work tree. It is
	// refused otherwise: a directory that the application can write is a
	// directory whose config, hooks and attributes it can plant, and the settings
	// of the Runner cannot neutralise all of that (docs/adr/0013). Whoever sets
	// it has decided that: docs/adr/0016 does so only for a Treebind without a
	// sub path.
	AllowGitDirInWorkTree bool

	// SSH says how Git connects to an ssh:// repository.
	SSH SSH

	// CABundle is a file of certificate authorities for https hosts, in place of
	// the system's. Empty means the system's bundle, which is looked for in the
	// usual places and handed to Git by name, because that is the only way to
	// keep the bundle that a repository config names out of use.
	CABundle string

	// Timeout is how long a command may run. DefaultTimeout when it is zero.
	Timeout time.Duration

	// MaxOutput is how many bytes a command may write to its standard output.
	// DefaultMaxOutput when it is zero.
	MaxOutput int64
}

// SSH holds what Git needs to connect to an ssh:// repository.
type SSH struct {
	// Program is the ssh binary. The first ssh on PATH is used when it is empty.
	Program string
	// IdentityFile is the private key. It is the only identity offered.
	IdentityFile string
	// KnownHostsFile lists the host keys that are trusted. It is the only list
	// consulted. With none, no host is trusted and every connection fails.
	KnownHostsFile string
}

// Result is what a command wrote.
type Result struct {
	// Stdout is the standard output, as it was written. It is data, and it is
	// not filtered: a command that prints a URL prints it as it is.
	Stdout []byte
	// Stderr is the end of the standard error, with the credentials of any URL
	// removed. It is for people: Git words it for them, and the wording changes
	// between releases.
	Stderr string
}

// Runner runs Git commands on one repository. It is safe for concurrent use,
// although Git's own locks make two commands that write the same repository
// wait for or fail on one another.
type Runner struct {
	git       string
	gitDir    string
	workTree  string
	path      string
	ssh       SSH
	caBundle  string
	timeout   time.Duration
	maxOutput int64

	// settings and env are computed once, because they never change.
	settings []string
	env      []string
}

// safePath is what a path that ends up in a command line of ssh may hold. The
// paths come from mounts and are known when the agent is built, so a character
// that a shell or ssh would read as something else is a mistake in the setup,
// and not something to quote around.
var safePath = regexp.MustCompile(`^/[A-Za-z0-9._/+@-]*$`)

// systemCABundles are the places where the usual systems keep their certificate
// authorities.
var systemCABundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// systemCABundle returns the first of them that exists, or "".
func systemCABundle() string {
	for _, path := range systemCABundles {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

// resolve returns the path with the symbolic links in it followed as far as it
// exists. The Git directory does not exist before the first init.
func resolve(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(resolve(parent), filepath.Base(path))
}

// New checks the options and returns a Runner. It looks at the file system for the
// links in the two directories and for the system's certificate authorities, and
// changes nothing: the Git directory does not exist before the first init.
func New(o Options) (*Runner, error) {
	for _, p := range []struct{ name, value string }{
		{"GitDir", o.GitDir},
		{"WorkTree", o.WorkTree},
	} {
		if err := checkDir(p.name, p.value); err != nil {
			return nil, err
		}
	}
	// Compared as the links lead, so that a link that points into the work tree is
	// not a way around.
	if rel, err := filepath.Rel(resolve(o.WorkTree), resolve(o.GitDir)); err == nil && !o.AllowGitDirInWorkTree &&
		rel != ".." && !strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("git: GitDir %q is inside WorkTree %q, where an application can write it; "+
			"keep it outside, or set AllowGitDirInWorkTree", o.GitDir, o.WorkTree)
	}
	for _, p := range []struct{ name, value string }{
		{"SSH.Program", o.SSH.Program},
		{"SSH.IdentityFile", o.SSH.IdentityFile},
		{"SSH.KnownHostsFile", o.SSH.KnownHostsFile},
		{"CABundle", o.CABundle},
	} {
		if p.value != "" && !safePath.MatchString(p.value) {
			return nil, fmt.Errorf("git: %s %q must be an absolute path of letters, digits and . _ / + @ -", p.name, p.value)
		}
	}
	if o.Timeout < 0 || o.MaxOutput < 0 {
		return nil, errors.New("git: Timeout and MaxOutput must not be negative")
	}

	r := &Runner{
		git:       o.Git,
		gitDir:    o.GitDir,
		workTree:  o.WorkTree,
		path:      defaultPath,
		ssh:       o.SSH,
		caBundle:  o.CABundle,
		timeout:   o.Timeout,
		maxOutput: o.MaxOutput,
	}
	if r.git == "" {
		git, err := exec.LookPath("git")
		if err != nil {
			return nil, fmt.Errorf("git: the git binary was not found: %w", err)
		}
		r.git = git
	}
	if r.ssh.Program == "" {
		// Not finding ssh is not an error here: a repository on https needs
		// none, and a Git that cannot run it says so when it is asked.
		r.ssh.Program = "ssh"
		if ssh, err := exec.LookPath("ssh"); err == nil {
			if !safePath.MatchString(ssh) {
				return nil, fmt.Errorf("git: ssh was found at %q, which has characters that a command line of ssh "+
					"cannot take; set SSH.Program to a path of letters, digits and . _ / + @ -", ssh)
			}
			r.ssh.Program = ssh
		}
	}
	if r.caBundle == "" {
		r.caBundle = systemCABundle()
	}
	if r.timeout == 0 {
		r.timeout = DefaultTimeout
	}
	if r.maxOutput == 0 {
		r.maxOutput = DefaultMaxOutput
	}
	r.settings = r.configArgs()
	r.env = r.environment()
	return r, nil
}

// checkDir requires an absolute, clean path that no Git setting can read as more
// than one directory. A "*" would turn safe.directory into a pattern.
func checkDir(name, path string) error {
	switch {
	case path == "":
		return fmt.Errorf("git: %s is required", name)
	case !filepath.IsAbs(path) || filepath.Clean(path) != path:
		return fmt.Errorf("git: %s %q must be an absolute, clean path", name, path)
	case strings.ContainsAny(path, "*\x00\n"):
		return fmt.Errorf("git: %s %q must not contain '*', a newline or a NUL", name, path)
	}
	return nil
}

// allowed are the subcommands the agent may run. Git runs a built-in command
// before it looks for an alias, so an alias in a repository config cannot take
// the place of any name here, and a name that is not here is refused before an
// alias could answer to it. Left out on purpose: diff, log and show, which run
// external diff programs and text converters; submodule and clone, which fetch
// submodules; gc and maintenance, which detach from the call; and every command
// that starts an editor, a mergetool or another program the agent does not use.
var allowed = map[string]bool{
	"add": true, "cat-file": true, "checkout": true, "checkout-index": true, "commit": true,
	"commit-tree": true, "diff-index": true, "diff-tree": true, "fetch": true, "for-each-ref": true,
	"hash-object": true, "init": true, "ls-files": true, "ls-remote": true, "ls-tree": true,
	"merge": true, "merge-base": true, "push": true, "read-tree": true, "reset": true,
	"rev-list": true, "rev-parse": true, "show-ref": true, "status": true, "symbolic-ref": true,
	"update-index": true, "update-ref": true, "version": true, "write-tree": true,
}

// Run runs a git subcommand, the first argument, with the rest of them, in the
// work tree, with the Runner's timeout. Put "--" before a URL or a path that comes
// from outside the agent, so that one that starts with a dash is not an option, and
// give a text as --message=<text> and not as a separate argument for the same
// reason.
func (r *Runner) Run(ctx context.Context, args ...string) (Result, error) {
	return r.RunTimeout(ctx, 0, args...)
}

// RunTimeout is Run with a timeout of its own, in place of the Runner's. It is for
// a command that is known to take long, like the first fetch of a large
// repository. Zero means the Runner's. The command is stopped when the context
// ends, whichever comes first.
func (r *Runner) RunTimeout(ctx context.Context, timeout time.Duration, args ...string) (Result, error) {
	if len(args) == 0 || !allowed[args[0]] {
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		return Result{}, &Error{Subcommand: name, Cause: ErrNotAllowed}
	}
	if option := refusedOption(args[0], args[1:]); option != "" {
		return Result{}, &Error{Subcommand: args[0], Option: option, Cause: ErrNotAllowed}
	}
	return r.run(ctx, timeout, args)
}

// refusedOptions are, for a subcommand, the options that make it sign, verify or
// run a program that the settings of the Runner cannot switch off, because the
// caller asked for it. --trailer runs the command that trailer.<token>.command
// names. Git takes any unambiguous start of a long option for it, so
// "--gpg" is "--gpg-sign". A short option can be one of a cluster, as in -qS.
// The long options that more than one subcommand refuses.
const (
	optGPGSign  = "gpg-sign"
	optTextconv = "textconv"
	optExec     = "exec"
)

var refusedOptions = map[string]struct {
	short bool
	long  []string
}{
	"commit":      {short: true, long: []string{optGPGSign, "trailer"}},
	"commit-tree": {short: true, long: []string{optGPGSign}},
	"merge":       {short: true, long: []string{optGPGSign, "verify-signatures"}},
	"push":        {long: []string{"signed", "receive-pack", optExec}},
	"fetch":       {long: []string{"upload-pack", optExec}},
	"ls-remote":   {long: []string{"upload-pack", optExec}},
	"diff-index":  {long: []string{"ext-diff", optTextconv, "output"}},
	"diff-tree":   {long: []string{"ext-diff", optTextconv, "output"}},
	"cat-file":    {long: []string{optTextconv, "filters"}},
}

// shortWithValue are the short options of those commands that take a value, which
// can follow in the same word, as in -mText. What follows is the value, not more
// options.
const shortWithValue = "mFCctusXp"

// refusedOption returns the first argument that the subcommand may not be given,
// or "". Nothing after "--" is an option. The Runner does not vet the other
// options: the arguments are the agent's own, and one that comes from outside has to
// be put after "--" or given as --name=value.
func refusedOption(subcommand string, args []string) string {
	refused := refusedOptions[subcommand]
	for _, arg := range args {
		if arg == "--" {
			return ""
		}
		if name, ok := strings.CutPrefix(arg, "--"); ok {
			name, _, _ = strings.Cut(name, "=")
			for _, long := range refused.long {
				if name != "" && strings.HasPrefix(long, name) {
					return arg
				}
			}
		} else if refused.short && signsShort(arg) {
			return arg
		}
	}
	return ""
}

// signsShort reports whether a word of short options has -S in it. The word ends at
// an option that takes a value, because the rest of it is that value.
func signsShort(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	for _, c := range arg[1:] {
		if c == 'S' {
			return true
		}
		if strings.ContainsRune(shortWithValue, c) {
			return false
		}
	}
	return false
}

// MinGitMajor and MinGitMinor are the oldest Git release whose options the Runner
// relies on: GIT_ATTR_SOURCE came with 2.40. An older Git ignores what it does not
// know, so the protections would be missing without a failure. The image of the
// agent carries a newer one, and CheckVersion asks the Git that is run.
const (
	MinGitMajor = 2
	MinGitMinor = 40
)

var gitVersion = regexp.MustCompile(`^git version (\d+)\.(\d+)`)

// CheckVersion returns an error when the Git that the Runner starts is older than
// MinGitMajor.MinGitMinor. The agent calls it once at start.
func (r *Runner) CheckVersion(ctx context.Context) error {
	result, err := r.Run(ctx, "version")
	if err != nil {
		return err
	}
	out := strings.TrimSpace(string(result.Stdout))
	m := gitVersion.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("git: cannot read a version from %s", quote(out))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < MinGitMajor || (major == MinGitMajor && minor < MinGitMinor) {
		return fmt.Errorf("git: %q is older than %d.%d, which the protections of the Runner need", out, MinGitMajor, MinGitMinor)
	}
	return nil
}

// run starts git and waits for it. It does not look at the subcommand, so a test
// can use the ones the agent does not.
func (r *Runner) run(ctx context.Context, timeout time.Duration, args []string) (Result, error) {
	if timeout <= 0 {
		timeout = r.timeout
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}

	ctx, cancelTimeout := context.WithTimeoutCause(ctx, timeout, ErrTimeout)
	defer cancelTimeout()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	stdout := &boundedBuffer{max: r.maxOutput, overflow: func() { cancel(ErrOutputLimit) }}
	stderr := &tailBuffer{max: stderrLimit}

	cmd := r.command(ctx, args)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Stdin is nil, which is the null device: nothing can be asked.

	err := cmd.Run()
	result := Result{Stdout: stdout.buf.Bytes(), Stderr: Redact(stderr.String())}
	return result, classify(ctx, sub, timeout, stdout, result.Stderr, err)
}

// command builds the command without starting it. The process gets a group of its
// own, so that a timeout can stop it and what it started, an ssh for one, and
// not only the process that Git started first.
func (r *Runner) command(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.git, slices.Concat(r.settings, args)...)
	cmd.Dir = r.workTree
	cmd.Env = r.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// A negative pid is the whole group.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = killGrace
	return cmd
}

// classify turns what happened into an Error, or into nil when the command
// succeeded within its limits. A command that wrote too much has failed even if it
// ended with status 0 before it could be stopped. Why the context ended decides
// next, because a killed command exits with a status that says nothing about why.
func classify(ctx context.Context, sub string, timeout time.Duration, stdout *boundedBuffer, stderr string, err error) error {
	// Git ended with status 0 but something that it started holds the output open.
	// The command itself succeeded.
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if err == nil && !stdout.over {
		return nil
	}
	e := &Error{Subcommand: sub, Stderr: errorStderr(stderr), Timeout: timeout, MaxOutput: stdout.max}
	switch cause := context.Cause(ctx); {
	case stdout.over:
		e.Cause = ErrOutputLimit
	case errors.Is(cause, ErrTimeout):
		e.Cause = ErrTimeout
	case ctx.Err() != nil:
		e.Cause = fmt.Errorf("%w: %w", ErrCanceled, cause)
	default:
		e.Cause = err
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			e.ExitCode = exit.ExitCode()
		}
	}
	return e
}

// errorStderr is the part of the standard error that goes into an error message:
// the last lines, on one line.
func errorStderr(stderr string) string {
	const limit = 2048
	if len(stderr) > limit {
		stderr = stderr[len(stderr)-limit:]
	}
	return strings.Join(strings.Fields(strings.ToValidUTF8(stderr, "")), " ")
}

// boundedBuffer keeps what is written to it, up to max bytes. Past that it keeps
// nothing, says so, and calls overflow once, which stops the command. It never
// fails a write: a pipe that nobody reads would block the command, and a
// command stopped for its output must not be able to wait for the reader.
type boundedBuffer struct {
	max      int64
	buf      bytes.Buffer
	over     bool
	overflow func()
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.over {
		return len(p), nil
	}
	if int64(b.buf.Len()+len(p)) > b.max {
		b.over = true
		b.overflow()
		return len(p), nil
	}
	return b.buf.Write(p)
}

// tailBuffer keeps the last max bytes that are written to it.
type tailBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = slices.Clone(t.buf[len(t.buf)-t.max:])
		t.truncated = true
	}
	return len(p), nil
}

// String returns what was kept. When the beginning was dropped the first line is
// cut off, whole: the cut can fall inside a URL, and the part of a URL that is
// left could be the credential without the scheme that redact looks for.
func (t *tailBuffer) String() string {
	s := string(t.buf)
	if t.truncated {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		} else {
			s = ""
		}
	}
	return s
}

// urlCredentials matches the userinfo of a URL, what stands between the scheme
// and the "@": a user name, a token or user:password. It reaches the last "@" of
// the word, because a password may hold a "/", a "?" or an "@" that Git prints as
// it is, and it would rather take a little too much of a URL than leave a part of
// a password. The user of an ssh URL is removed with the rest, because there is no
// telling which is which.
var urlCredentials = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s'"]*@`)

// urlQuery matches the query of a URL. A host can take a token there.
var urlQuery = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s?#]*)\?[^\s#'"]*`)

// redact removes what looks like a credential in a URL: its userinfo and its
// query. Git prints the URLs it fails on, and a person could have put a token in
// one.
func Redact(s string) string {
	s = urlCredentials.ReplaceAllString(s, "${1}<redacted>@")
	return urlQuery.ReplaceAllString(s, "${1}?<redacted>")
}
