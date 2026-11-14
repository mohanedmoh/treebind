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
	"strings"
)

// emptyTree is the object name of the empty tree in a SHA-1 repository. Git knows
// it without the object being stored, so it works in any repository.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// nullDevice is where Git reads nothing from, and what a file that must not be
// read is pointed at.
const nullDevice = "/dev/null"

// falseValue is the value of the settings that switch something off.
const falseValue = "false"

// setting is one configuration value that the Runner passes on the command line
// of every Git command. A value given with -c is read after every configuration
// file, so it beats whatever the repository's own config says, including what
// that config includes.
type setting struct {
	key, value string
	// why says what a repository config could otherwise do. The Git releases
	// that the keys need are in docs/adr/0013.
	why string
}

// fixedSettings are the same for every Runner. safe.directory depends on the work
// tree and is added in New. A key that Git does not know is ignored, so a setting
// never breaks an older Git; it only stops protecting.
//
// Only keys that can be overridden by name are here. What cannot be, because the
// name is chosen by the repository (filter.<name>, diff.<name>, merge.<name>),
// is switched off at its root: the attributes that select a driver are read from
// an empty tree (GIT_ATTR_SOURCE), and the subcommands that show a diff or run a
// merge tool are not in the allowlist.
var fixedSettings = []setting{
	{"core.hooksPath", nullDevice, "hooks would run with the agent's credentials"},
	{"core.fsmonitor", "", "names a program that status and add run, or starts a daemon"},
	{"core.askPass", "", "names a program that Git runs to ask for a password"},
	{"credential.helper", "", "names programs that receive the credentials; an empty value resets every helper " +
		"set before it, scoped to a URL or not"},
	{"core.symlinks", "true", "a link is a link on every volume, and a repository config cannot turn that off"},
	{"core.autocrlf", falseValue, "a staged blob has to be the bytes of the file (#94)"},
	{"core.attributesFile", nullDevice, "would read attributes, and so filters, from a file of its choosing"},
	{"core.excludesFile", nullDevice, "would read ignore patterns from a file of its choosing"},
	{"protocol.allow", "never", "no transport that is not named below"},
	{"protocol.ssh.allow", "always", "the one transport that carries a key"},
	{"protocol.https.allow", "always", "the transport for a token or a public repository"},
	{"http.sslVerify", "true", "the certificate of a Git host is always verified"},
	{"http.proxy", "", "an empty value sends nothing through a proxy that the config names"},
	{"http.extraHeader", "", "an empty value resets the headers that the config adds to every request"},
	{"submodule.recurse", falseValue, "commands do not enter submodules"},
	{"fetch.recurseSubmodules", falseValue, "a fetch does not fetch submodules"},
	{"diff.ignoreSubmodules", "all", "status does not run Git inside a submodule"},
	{"commit.gpgSign", falseValue, "would run the signing program on every commit and merge"},
	{"tag.gpgSign", falseValue, "would run the signing program on every tag"},
	{"push.gpgSign", falseValue, "would run the signing program on every push"},
	{"merge.verifySignatures", falseValue, "would run the verifying program on every commit that a merge brings in"},
	{"gpg.format", "openpgp", "selects which of the programs below runs, so that one that is not nulled is not reached"},
	{"gpg.program", nullDevice, "names the program that signs and verifies"},
	{"gpg.openpgp.program", nullDevice, "names the program that signs and verifies with OpenPGP, before gpg.program"},
	{"gpg.ssh.program", nullDevice, "names the program that signs and verifies with an ssh key"},
	{"gpg.x509.program", nullDevice, "names the program that signs and verifies with a certificate"},
	{"gpg.ssh.defaultKeyCommand", "", "names a program that finds the key to sign with"},
	{"gc.auto", "0", "a commit, merge or fetch would start a gc that detaches from the call and from its process group"},
	{"maintenance.auto", falseValue, "a commit, merge or fetch would start maintenance tasks that the config selects"},
}

// allowedProtocols are the transports Git may use. GIT_ALLOW_PROTOCOL replaces
// every protocol.<name>.allow in the configuration, so a repository config that
// sets protocol.file.allow=always still cannot reach a file, git or ext URL.
const allowedProtocols = "ssh:https"

// configArgs returns the -c options of every command.
func (r *Runner) configArgs() []string {
	args := make([]string, 0, 2*(len(fixedSettings)+1))
	for _, s := range fixedSettings {
		args = append(args, "-c", s.key+"="+s.value)
	}
	// Git skips its ownership check when GIT_DIR is set, which the Runner always
	// does, so this value is only reached by a command that finds its repository
	// by walking up from the current directory. It names the work tree and never
	// "*", so it does not extend to another directory.
	return append(args, "-c", "safe.directory="+r.workTree)
}

// environment builds the environment of every command from nothing. Nothing is
// inherited: a GIT_* variable, a proxy, a locale, a home directory or an
// askpass program of the agent's own environment never reaches Git.
func (r *Runner) environment() []string {
	env := []string{
		// Where Git looks for programs it starts itself. The exec path of Git is
		// added in front by Git.
		"PATH=" + r.path,
		// Git and ssh read nothing from the home directory that is named here.
		"HOME=/nonexistent",
		// Plain, parsable messages whatever the container's locale is.
		"LC_ALL=C",
		"GIT_DIR=" + r.gitDir,
		"GIT_WORK_TREE=" + r.workTree,
		// An explicit global and system file replace the lookups in HOME and
		// /etc, whatever the configuration of the image holds.
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		// A prompt would wait for a person who is not there.
		"GIT_TERMINAL_PROMPT=0",
		// Neither of these is ever started: the output is a pipe, and every
		// message the agent writes has its text given.
		"GIT_EDITOR=:",
		"GIT_PAGER=cat",
		// Beats core.sshCommand, which a repository config could set.
		"GIT_SSH_COMMAND=" + sshCommand(r.ssh),
		"GIT_ALLOW_PROTOCOL=" + allowedProtocols,
		// The attributes of the work tree select filters, diff drivers and
		// end-of-line conversion. Reading them from an empty tree applies none.
		"GIT_ATTR_SOURCE=" + emptyTree,
	}
	if r.caBundle != "" {
		// Read after http.sslCAInfo, so it beats a repository config, which an
		// empty setting cannot (it breaks https to every host).
		env = append(env, "GIT_SSL_CAINFO="+r.caBundle)
	}
	return env
}

// sshCommand is the command Git runs as ssh. A repository config cannot change
// it, because GIT_SSH_COMMAND comes before core.sshCommand. The host is verified
// only against the known hosts file that was given, and a connection to a host
// that it does not list fails: with no file given the list is empty, so a
// mistake in the setup fails closed (ADR 0005). Nothing is read from the system
// or the user's ssh configuration (-F), no key comes from an agent or the home
// directory (IdentitiesOnly), no question is asked (BatchMode), and the known
// hosts are never written (UpdateHostKeys).
func sshCommand(s SSH) string {
	known := s.KnownHostsFile
	if known == "" {
		known = nullDevice
	}
	args := []string{
		s.Program,
		"-F", nullDevice,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UpdateHostKeys=no",
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "UserKnownHostsFile=" + known,
		"-o", "IdentitiesOnly=yes",
	}
	if s.IdentityFile != "" {
		args = append(args, "-i", s.IdentityFile)
	} else {
		// IdentitiesOnly leaves the default identity files of the user in use when
		// none is named, and ssh finds them through the passwd file, not through
		// HOME. A file that holds no key leaves it with nothing to offer.
		args = append(args, "-o", "IdentityFile="+nullDevice)
	}
	return strings.Join(args, " ")
}
