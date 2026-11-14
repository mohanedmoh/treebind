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

package git

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// widenProtocols is what a repository config would write to let Git use any
// transport. GIT_ALLOW_PROTOCOL has to win over all of it.
var widenProtocols = map[string]string{
	"protocol.allow":      valueAlways,
	"protocol.file.allow": valueAlways,
	"protocol.ext.allow":  valueAlways,
	"protocol.git.allow":  valueAlways,
	"protocol.http.allow": valueAlways,
}

func TestOnlySSHAndHTTPSCanBeUsed(t *testing.T) {
	for _, l := range layouts {
		t.Run(string(l), func(t *testing.T) {
			r := newRepo(t, l)
			_, remote := r.remote()
			extMarker := r.marker("ext")

			refused := map[string]string{
				"a file URL":       "file://" + remote,
				"a local path":     remote,
				"the git protocol": urlGitProto,
				"plain http":       "http://127.0.0.1:1/r.git",
				"an ext address":   "ext::sh -c echo% ran% >>% " + extMarker + " %S",
			}
			for key, value := range widenProtocols {
				r.config(key, value)
			}
			runner := r.runner()
			for name, url := range refused {
				t.Run("refuses "+name, func(t *testing.T) {
					_, err := r.run(runner, cmdLsRemote, "--", url)
					wantGitFailure(t, err, msgNotAllowed)
				})
			}
			wantNotRan(t, extMarker, "the ext:: command")

			// The unprotected control reads the same file URL, so the refusals above
			// are the Runner's doing and not a mistake in the URL.
			if out, err := r.plainErr(r.env(), cmdLsRemote, "file://"+remote); err != nil {
				t.Fatalf("plain Git cannot read the file URL, so the test proves nothing: %v: %s", err, out)
			}
		})
	}
}

func TestSSHAndHTTPSPassTheProtocolCheck(t *testing.T) {
	r := newRepo(t, outsideWork)
	fake, log := r.fakeSSH()
	url, _ := r.remote()
	runner := r.runner(func(o *Options) { o.SSH.Program = fake })

	t.Run("an ssh URL is served through the ssh program", func(t *testing.T) {
		result, err := r.run(runner, cmdLsRemote, "--", url)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(result.Stdout), "refs/heads/main") {
			t.Errorf("the refs were not listed: %q", result.Stdout)
		}
		if !exists(log) {
			t.Error("Git did not run the ssh program")
		}
	})

	t.Run("an https URL is tried, and fails to connect, not to pass the check", func(t *testing.T) {
		_, err := r.run(runner, cmdLsRemote, "--", "https://127.0.0.1:1/r.git")
		wantGitFailure(t, err, "") // Git ran, and failed
		if strings.Contains(err.Error(), msgNotAllowed) {
			t.Errorf("the https transport was refused: %v", err)
		}
	})
}

func TestARedirectDoesNotLeaveTheAllowedProtocols(t *testing.T) {
	r := newRepo(t, outsideWork)
	var plainRequests atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainRequests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(plain.Close)
	web := newWebServer(t, r.root, func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, plain.URL+req.URL.Path, http.StatusFound)
	})
	url := web.URL + "/r.git"

	_, _ = r.plainErr(r.env("GIT_SSL_CAINFO="+web.CABundle), cmdLsRemote, url)
	if plainRequests.Load() == 0 {
		t.Fatal("plain Git did not follow the redirect, so the test proves nothing")
	}

	plainRequests.Store(0)
	_, err := r.run(r.runner(func(o *Options) { o.CABundle = web.CABundle }), cmdLsRemote, "--", url)
	if err == nil {
		t.Fatal("the redirect to http was followed")
	}
	if n := plainRequests.Load(); n != 0 {
		t.Errorf("the redirect to http was followed: %d requests", n)
	}
}

func TestHTTPSSettingsOfARepositoryConfigDoNotApply(t *testing.T) {
	t.Run("a certificate is verified", func(t *testing.T) {
		r := newRepo(t, insideWork)
		web := newWebServer(t, r.root, notFound)
		r.config("http.sslVerify", "false")

		_, _ = r.plainErr(r.env(), cmdLsRemote, web.URL+"/r.git")
		if len(web.seen()) == 0 {
			t.Fatal("plain Git did not skip the verification, so the test proves nothing")
		}
		before := len(web.seen())

		_, err := r.run(r.runner(), cmdLsRemote, "--", web.URL+"/r.git")
		wantGitFailure(t, err, "")
		if got := len(web.seen()); got != before {
			t.Errorf("the server was asked %d times after the certificate should have been refused", got-before)
		}
	})

	t.Run("no proxy is used", func(t *testing.T) {
		r := newRepo(t, insideWork)
		web := newWebServer(t, r.root, notFound)
		var connections atomic.Int32
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				connections.Add(1)
				_ = conn.Close()
			}
		}()
		r.config("http.proxy", "http://"+listener.Addr().String())

		_, _ = r.plainErr(r.env("GIT_SSL_CAINFO="+web.CABundle), cmdLsRemote, web.URL+"/r.git")
		if connections.Load() == 0 {
			t.Fatal("plain Git did not use the proxy, so the test proves nothing")
		}

		connections.Store(0)
		_, err = r.run(r.runner(func(o *Options) { o.CABundle = web.CABundle }), cmdLsRemote, "--", web.URL+"/r.git")
		wantGitFailure(t, err, "not found")
		if n := connections.Load(); n != 0 {
			t.Errorf("%d connections went to the proxy of the repository config", n)
		}
	})

	t.Run("no header is added", func(t *testing.T) {
		r := newRepo(t, insideWork)
		web := newWebServer(t, r.root, notFound)
		r.config("http.extraHeader", "X-Planted: yes")

		_, _ = r.plainErr(r.env("GIT_SSL_CAINFO="+web.CABundle), cmdLsRemote, web.URL+"/r.git")
		if len(web.seen()) == 0 || web.seen()[0].Header.Get("X-Planted") == "" {
			t.Fatal("plain Git did not send the header, so the test proves nothing")
		}
		before := len(web.seen())

		_, err := r.run(r.runner(func(o *Options) { o.CABundle = web.CABundle }), cmdLsRemote, "--", web.URL+"/r.git")
		wantGitFailure(t, err, "not found")
		for _, req := range web.seen()[before:] {
			if req.Header.Get("X-Planted") != "" {
				t.Errorf("a request carried the header of the repository config")
			}
		}
	})

	t.Run("the system's authorities are used when the Runner has no bundle", func(t *testing.T) {
		if systemCABundle() == "" {
			t.Skip("this system has no bundle of certificate authorities in a usual place, so the Runner cannot name it")
		}
		r := newRepo(t, insideWork)
		web := newWebServer(t, r.root, notFound)
		r.config("http.sslCAInfo", web.CABundle)

		_, _ = r.plainErr(r.env(), cmdLsRemote, web.URL+"/r.git")
		if len(web.seen()) == 0 {
			t.Fatal("plain Git did not trust the bundle of the config, so the test proves nothing")
		}
		before := len(web.seen())

		_, err := r.run(r.runner(), cmdLsRemote, "--", web.URL+"/r.git")
		wantGitFailure(t, err, "")
		if got := len(web.seen()); got != before {
			t.Errorf("the server was asked %d times: the bundle of the repository config was trusted", got-before)
		}
	})

	t.Run("the CA bundle of the Runner is the one used, not the one of the repository config", func(t *testing.T) {
		r := newRepo(t, insideWork)
		web := newWebServer(t, r.root, notFound)
		other := newWebServer(t, tempDir(t), notFound)
		r.config("http.sslCAInfo", other.CABundle)

		_, err := r.run(r.runner(func(o *Options) { o.CABundle = web.CABundle }), cmdLsRemote, "--", web.URL+"/r.git")
		wantGitFailure(t, err, "not found")
	})
}

func TestSSHRunsTheFixedCommand(t *testing.T) {
	r := newRepo(t, outsideWork)
	fake, log := r.fakeSSH()
	url, _ := r.remote()
	key := filepath.Join(r.root, "key")
	known := filepath.Join(r.root, "known_hosts")
	runner := r.runner(func(o *Options) {
		o.SSH = SSH{Program: fake, IdentityFile: key, KnownHostsFile: known}
	})

	if _, err := r.run(runner, cmdLsRemote, "--", url); err != nil {
		t.Fatal(err)
	}
	called := readFile(t, log)
	for _, want := range []string{
		"-F /dev/null", "BatchMode=yes", "StrictHostKeyChecking=yes", "UserKnownHostsFile=" + known,
		"GlobalKnownHostsFile=/dev/null", "IdentitiesOnly=yes", "-i " + key,
	} {
		if !strings.Contains(called, want) {
			t.Errorf("ssh was called without %q: %s", want, called)
		}
	}
}

func TestTheProtocolSettingsAreEnoughWithoutTheEnvironmentVariable(t *testing.T) {
	// GIT_ALLOW_PROTOCOL replaces the settings, so no other test sees them. A Git
	// that ignored the variable would still have to refuse.
	r := newRepo(t, outsideWork)
	fake, _ := r.fakeSSH()
	url, remote := r.remote()
	runner := r.runner(func(o *Options) { o.SSH.Program = fake })
	ls := func(target string) error {
		cmd := runner.command(context.Background(), []string{cmdLsRemote, "--", target})
		var env []string
		for _, e := range cmd.Env {
			if !strings.HasPrefix(e, "GIT_ALLOW_PROTOCOL=") {
				env = append(env, e)
			}
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}

	for name, target := range map[string]string{
		"a file URL": "file://" + remote, "a local path": remote, "the git protocol": urlGitProto,
		"plain http": "http://127.0.0.1:1/r.git", "an ext address": "ext::sh -c true %S",
	} {
		if err := ls(target); err == nil || !strings.Contains(err.Error(), msgNotAllowed) {
			t.Errorf("%s: got %v, want it refused", name, err)
		}
	}
	if err := ls(url); err != nil {
		t.Errorf("an ssh URL was refused: %v", err)
	}
	if err := ls("https://127.0.0.1:1/r.git"); err == nil || strings.Contains(err.Error(), msgNotAllowed) {
		t.Errorf("an https URL was refused, or succeeded: %v", err)
	}
}
