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
// It checks the volume it works on and serves its status to the operator. The
// sync loop is not implemented yet, so the agent never reports itself ready.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mohanedmoh/treebind/internal/agent/app"
	"github.com/mohanedmoh/treebind/internal/agent/auth"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/umask"
)

// The exit codes.
const (
	exitOK     = 0
	exitFailed = 1
	// exitConfig is for a pod that is set up wrongly, which a restart does not cure.
	exitConfig = 2
)

func main() {
	umask.Set()

	// SIGTERM is the only warning the agent gets that its pod is going away.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	code := run(ctx, os.Getenv, os.Stderr)
	stop()
	os.Exit(code)
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

	if err := app.Run(ctx, app.Options{Config: settings, Verifier: verifier, Logger: logger}); err != nil {
		logger.Error("the agent stopped", "error", err)
		return exitFailed
	}
	logger.Info("the agent stopped on request")
	return exitOK
}
