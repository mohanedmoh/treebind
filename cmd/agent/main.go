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

// Command agent is the Treebind sync agent.
//
// It checks the volume it works on, makes the checkout of the repository or opens
// the one that is there, pulls into it, and, for a Treebind that syncs in both directions, looks for what
// the application changed, commits what has settled, pushes the commits, and serves its status to the operator.
// It reports itself ready once a pull has passed or a push has worked.
//
// Started with the arguments write-passwd and a path, it writes the passwd file of the sync container there and exits
// (docs/adr/0042).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mohanedmoh/treebind/internal/agent/app"
	"github.com/mohanedmoh/treebind/internal/agent/auth"
	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/commit"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/maintain"
	"github.com/mohanedmoh/treebind/internal/agent/passwd"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/push"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/status"
	"github.com/mohanedmoh/treebind/internal/agent/umask"
	"github.com/mohanedmoh/treebind/internal/agent/watch"
)

// The exit codes.
const (
	exitOK     = 0
	exitFailed = 1
	// exitConfig is for a pod that is set up wrongly, which a restart does not cure.
	exitConfig = 2
	// exitLockLost is for an agent that lost the writer lock to another one. It has stopped what it
	// was doing, and the restart of its pod waits for the lock again. It is the code of a lost lock also when
	// the agent was told to stop as well, so that a loss never looks like a clean stop.
	exitLockLost = 3
)

// acquireLock takes the writer lock of the volume. A test replaces it, so that no test of the agent's main
// writes to a volume.
var acquireLock = lock.Acquire

// openCheckout makes the checkout, or opens the one that is there. A test replaces it, so
// that no test starts Git against a repository.
var openCheckout = checkout.Open

// newPuller makes what pulls into the checkout. A test replaces it, so that no test starts Git against a
// repository.
var newPuller = func(options pull.Options) (app.Puller, error) {
	puller, err := pull.New(options)
	if err != nil {
		return nil, err
	}
	return puller, nil
}

// newScanner makes what scans the directory for local changes. A test replaces it, so that no test starts
// Git against a repository.
var newScanner = func(options scan.Options) (app.Scanner, error) {
	scanner, err := scan.New(options)
	if err != nil {
		return nil, err
	}
	return scanner, nil
}

// newCommitter makes what commits the changes that have settled. A test replaces it, so that no test starts Git
// against a repository.
var newCommitter = func(options commit.Options) (app.Committer, error) {
	committer, err := commit.New(options)
	if err != nil {
		return nil, err
	}
	return committer, nil
}

// newPusher makes what pushes the commits of the checkout. A test replaces it, so that no test starts Git against a
// repository.
var newPusher = func(options push.Options) (app.Pusher, error) {
	pusher, err := push.New(options)
	if err != nil {
		return nil, err
	}
	return pusher, nil
}

// newMaintainer makes what packs the objects of the Git directory. A test replaces it, so that no test starts Git
// against a repository.
var newMaintainer = func(options maintain.Options) (app.Maintainer, error) {
	maintainer, err := maintain.New(options)
	if err != nil {
		return nil, err
	}
	return maintainer, nil
}

// newWatcher makes what tells the agent that the directory may have changed. A test replaces it, so that no
// test puts watches on a directory.
var newWatcher = func(options watch.Options) (app.Watcher, error) {
	watcher, err := watch.New(options)
	if err != nil {
		return nil, err
	}
	return watcher, nil
}

func main() {
	umask.Set()

	if len(os.Args) > 1 {
		os.Exit(runCommand(os.Args[1:], os.Stderr))
	}

	// SIGTERM is the only warning the agent gets that its pod is going away.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	code := run(ctx, os.Getenv, os.Stderr)
	stop()
	os.Exit(code)
}

// passwdSource is the passwd file of the image, which the one that the agent writes starts from. A test replaces it.
var passwdSource = passwd.SystemFile

// runCommand runs the agent with arguments, which it takes only to write the passwd file of the sync container. The
// init container of the sync pod runs it as the user that the platform gives the pod, before the sync container starts
// with the file mounted over /etc/passwd, so that ssh finds that user (docs/adr/0042).
func runCommand(args []string, stderr io.Writer) int {
	if len(args) != 2 || args[0] != passwd.Command || !filepath.IsAbs(args[1]) {
		_, _ = fmt.Fprintf(stderr, "treebind-agent: the arguments %q are wrong: the agent takes none, or %s and "+
			"the absolute path of the file to write\n", args, passwd.Command)
		return exitConfig
	}
	if err := passwd.Write(passwdSource, args[1]); err != nil {
		_, _ = fmt.Fprintf(stderr, "treebind-agent: %v\n", err)
		return exitFailed
	}
	return exitOK
}

// agentOptions is what the agent runs with: its settings, the check of the operator's token,
// the checkout that it makes once the volume is in order, the pulls that follow it, and the scans for
// local changes, the commits of what they find settled and the pushes of the commits, that go on beside them
// for a Treebind that syncs in both directions.
func agentOptions(settings config.Config, verifier status.Verifier, logger *slog.Logger) app.Options {
	return app.Options{
		Config: settings, Verifier: verifier, Logger: logger, AcquireLock: acquireLock, Checkout: openCheckout,
		NewPuller: newPuller, NewScanner: newScanner, NewWatcher: newWatcher, NewCommitter: newCommitter,
		NewPusher: newPusher, NewMaintainer: newMaintainer,
	}
}

// exitCodeOf is the exit code for an error of the agent that stopped: its own for the loss of the
// lock, which is not a fault of the pod, and the general one for the rest.
func exitCodeOf(err error) int {
	if errors.Is(err, app.ErrLockLost) {
		return exitLockLost
	}
	return exitFailed
}

// run does what main does, with its environment and its output passed in.
func run(ctx context.Context, getenv func(string) string, stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	settings, err := config.FromEnv(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "treebind-agent: the configuration is wrong:\n%v\n", err)
		return exitConfig
	}
	verifier, err := auth.New(auth.Config{
		Issuer:   settings.StatusIssuer,
		Audience: settings.StatusAudience,
		Subject:  settings.StatusSubject,
		KeysFile: settings.StatusKeysFile,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "treebind-agent: %v\n", err)
		return exitConfig
	}

	if err := app.Run(ctx, agentOptions(settings, verifier, logger)); err != nil {
		logger.Error("the agent stopped", "error", err)
		return exitCodeOf(err)
	}
	logger.Info("the agent stopped on request")
	return exitOK
}
