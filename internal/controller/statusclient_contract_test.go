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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"

	"github.com/mohanedmoh/treebind/internal/agent/auth"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/status"
	"github.com/mohanedmoh/treebind/internal/statusclient"
)

// The client calls a port that is written in the client, and the agent listens on
// the port that the operator writes in its pod. Nothing but this test says that the
// two are the same.
func TestTheClientCallsThePortTheAgentListensOn(t *testing.T) {
	container, _ := agentContainer(t, newTreebind(testTreebindName))

	var listens string
	for _, variable := range container.Env {
		if variable.Name == agentconfig.EnvStatusAddr {
			listens = variable.Value
		}
	}
	if want := fmt.Sprintf(":%d", statusclient.Port); listens != want {
		t.Errorf("the agent is told to listen on %q, but the client calls port %d", listens, statusclient.Port)
	}
	if len(container.Ports) != 1 || container.Ports[0].ContainerPort != statusclient.Port {
		t.Errorf("the container declares %+v, want the port %d that the client calls", container.Ports, statusclient.Port)
	}
	if statusclient.Port != statusPort {
		t.Errorf("the client calls port %d, the pod template uses %d", statusclient.Port, statusPort)
	}
}

// The operator mints a token and calls the agent with it, and the agent checks the
// token itself. Each side is tested alone elsewhere. This puts the real pieces
// against a real API server: the operator's token source and client on one side,
// and on the other the agent's handler and verifier, configured from the
// environment of the pod that the operator builds, with the cluster's own keys.
// What the two have to agree on, the audience, the subject, the issuer and the
// keys, is wired the way it is in a cluster, so a change to one side that the other
// does not follow fails here.
var _ = Describe("The operator and the agent", func() {
	const (
		namespace      = "default"
		operatorName   = "contract-operator"
		strangerName   = "contract-stranger"
		treebindName   = "contract"
		reportedChecks = "Mount"
	)

	var (
		ctx      = context.Background()
		treebind = newTreebind(treebindName)
		subject  = "system:serviceaccount:" + namespace + ":" + operatorName
	)

	// get reads a path of the API server, as the operator does.
	get := func(path string) []byte {
		GinkgoHelper()
		client, err := discovery.NewDiscoveryClientForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		body, err := client.RESTClient().Get().AbsPath(path).DoRaw(ctx)
		Expect(err).NotTo(HaveOccurred())
		return body
	}

	BeforeEach(func() {
		for _, name := range []string{operatorName, strangerName} {
			Expect(k8sClient.Create(ctx, &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			})).To(Succeed())
		}
	})

	AfterEach(func() {
		for _, name := range []string{operatorName, strangerName} {
			Expect(k8sClient.Delete(ctx, &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			})).To(Succeed())
		}
	})

	// agentServer starts the agent's own handler, with the verifier configured from
	// the environment of the sync pod that the operator would create for the
	// Treebind, and the keys that the API server publishes.
	agentServer := func() *httptest.Server {
		GinkgoHelper()
		var discovered struct {
			Issuer string `json:"issuer"`
		}
		Expect(json.Unmarshal(get("/.well-known/openid-configuration"), &discovered)).To(Succeed())

		statefulSet, err := desiredStatefulSet(treebind, agentSettings{
			Image: testImage, StatusIssuer: discovered.Issuer, StatusSubject: subject,
		})
		Expect(err).NotTo(HaveOccurred())
		env := map[string]string{}
		for _, variable := range statefulSet.Spec.Template.Spec.Containers[0].Env {
			env[variable.Name] = variable.Value
			if variable.ValueFrom != nil {
				env[variable.Name] = namespace // metadata.namespace, the only field the pod reads
			}
		}
		config, err := agentconfig.FromEnv(func(name string) string { return env[name] })
		Expect(err).NotTo(HaveOccurred(), "the agent refuses the environment of its own pod")

		keysFile := filepath.Join(GinkgoT().TempDir(), "jwks.json")
		Expect(os.WriteFile(keysFile, get("/openid/v1/jwks"), 0o600)).To(Succeed())
		verifier, err := auth.New(auth.Config{
			Issuer: config.StatusIssuer, Audience: config.StatusAudience, Subject: config.StatusSubject, KeysFile: keysFile,
		})
		Expect(err).NotTo(HaveOccurred())

		report := status.Report{
			Treebind:  status.Treebind{Namespace: config.Namespace, Name: config.Name},
			StartedAt: time.Now().Add(-time.Hour),
			Checks:    []status.Check{{Name: reportedChecks, Outcome: "Passed", Message: "mounted"}},
		}
		server := httptest.NewServer(status.NewHandler(func() status.Report { return report }, verifier))
		DeferCleanup(server.Close)
		return server
	}

	// clientAs returns a client that calls the server with tokens of the service
	// account.
	clientAs := func(server *httptest.Server, account string) *statusclient.Client {
		client := statusclient.New(&statusclient.ServiceAccountTokens{
			Client: k8sClient, Namespace: namespace, ServiceAccount: account,
		})
		client.Address = func(statusclient.Target) (string, error) {
			return strings.TrimPrefix(server.URL, "http://"), nil
		}
		return client
	}

	expectKind := func(err error, kind statusclient.Kind) {
		GinkgoHelper()
		var fetchErr *statusclient.Error
		Expect(errors.As(err, &fetchErr)).To(BeTrue(), "error = %v", err)
		Expect(fetchErr.Kind).To(Equal(kind))
	}

	target := func() statusclient.Target {
		return statusclient.Target{Namespace: treebind.Namespace, Name: treebind.Name, Audience: statusAudience(treebind)}
	}

	It("reads the report with a token that the operator minted for the agent of the Treebind", func() {
		server := agentServer()

		report, err := clientAs(server, operatorName).Fetch(ctx, target())

		Expect(err).NotTo(HaveOccurred())
		Expect(report.Treebind).To(Equal(status.Treebind{Namespace: namespace, Name: treebindName}))
		Expect(report.Checks).To(HaveLen(1))
		Expect(report.Checks[0].Name).To(Equal(reportedChecks))
	})

	It("is refused with a token that was minted for the agent of another Treebind", func() {
		server := agentServer()
		other := target()
		other.Audience = statusAudience(newTreebind("another"))

		_, err := clientAs(server, operatorName).Fetch(ctx, other)

		expectKind(err, statusclient.KindTokenRefused)
	})

	It("is refused when the token is another service account's, whatever its audience", func() {
		server := agentServer()

		_, err := clientAs(server, strangerName).Fetch(ctx, target())

		expectKind(err, statusclient.KindTokenRefused)
	})
})
