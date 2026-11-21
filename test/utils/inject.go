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

package utils

import (
	"os/exec"
	"strings"
)

// The functions below run the verbs of hack/inject.sh, which causes a failure in a container from the node
// that runs it, through a privileged pod with hostPID that it starts there when there is none. The script
// refuses a kubectl context that is not a kind cluster. Each returns what the script says it did: the pid,
// the ID of the container and its cgroup, for the log of the spec.

// KillContainer sends SIGKILL to the first process of a container of a pod, as an out-of-memory kill
// does. The kubelet starts the container again in the same pod.
func KillContainer(namespace, pod, container string) (string, error) {
	return inject("kill", namespace, pod, container)
}

// FreezeContainer freezes every process of a container of a pod with the freezer of cgroup v2, until
// ThawContainer lets them run again.
func FreezeContainer(namespace, pod, container string) (string, error) {
	return inject("freeze", namespace, pod, container)
}

// ThawContainer lets the processes of a container that FreezeContainer froze run again.
func ThawContainer(namespace, pod, container string) (string, error) {
	return inject("thaw", namespace, pod, container)
}

// RemoveInjector removes the privileged pods that the verbs start, with their namespace, and waits for
// them to be gone.
func RemoveInjector() error {
	_, err := Run(exec.Command("hack/inject.sh", "cleanup"))
	return err
}

func inject(verb, namespace, pod, container string) (string, error) {
	output, err := Run(exec.Command("hack/inject.sh", verb, namespace, pod, container))
	return strings.TrimSpace(output), err
}
