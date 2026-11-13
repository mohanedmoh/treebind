//go:build e2e
// +build e2e

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

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mohanedmoh/treebind/test/utils"
)

// namespace where the project is deployed in
const namespace = "treebind-system"

// serviceAccountName created for the project
const serviceAccountName = "treebind-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "treebind-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "treebind-metrics-binding"

// The operator runs as two replicas that elect a leader through a Lease, and a
// PodDisruptionBudget lets a drain evict one of them at a time (NFR-21).
const (
	controllerReplicas      = 2
	leaseName               = "c4984a8a.example.org"
	podDisruptionBudgetName = "treebind-controller-manager"
	controllerPodSelector   = "control-plane=controller-manager"
)

// The objects of the sync workload spec, which runs in the default namespace
// because the pod of a Treebind has to share the namespace of its claim.
const (
	syncNamespace       = "default"
	syncTreebindName    = "e2e-sync"
	syncClaimName       = "e2e-sync-data"
	syncKeySecretName   = "e2e-sync-key"
	syncHostsMapName    = "e2e-sync-hosts"
	syncStatefulSetName = syncTreebindName + "-sync"
	syncKeysName        = syncStatefulSetName + "-keys"
	syncMarkerPodName   = "e2e-sync-marker"
	syncMarkerContent   = "kept"
)

// syncMarkerManifest is a second pod that writes a file to the claim, so a test
// can check that the data is still there after the Treebind is gone.
const syncMarkerManifest = `apiVersion: v1
kind: Pod
metadata:
  name: ` + syncMarkerPodName + `
  namespace: ` + syncNamespace + `
spec:
  containers:
  - name: marker
    image: curlimages/curl:latest
    command: ["/bin/sh", "-c", "echo ` + syncMarkerContent + ` > /data/marker && sleep 3600"]
    securityContext:
      runAsUser: 0
    volumeMounts:
    - {name: data, mountPath: /data}
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: ` + syncClaimName + `
`

// syncManifest is the claim, the SSH key and known hosts an ssh:// repository
// needs, and the smallest Treebind that names them.
const syncManifest = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ` + syncClaimName + `
  namespace: ` + syncNamespace + `
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Secret
metadata:
  name: ` + syncKeySecretName + `
  namespace: ` + syncNamespace + `
stringData:
  id_ed25519: not-a-real-key
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + syncHostsMapName + `
  namespace: ` + syncNamespace + `
data:
  known_hosts: github.com ssh-ed25519 AAAA
---
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata:
  name: ` + syncTreebindName + `
  namespace: ` + syncNamespace + `
spec:
  repository:
    url: ssh://git@github.com/example/dashboards.git
    branch: main
    auth:
      sshKeySecretRef: {name: ` + syncKeySecretName + `, key: id_ed25519}
      knownHostsConfigMapRef: {name: ` + syncHostsMapName + `, key: known_hosts}
  target:
    claimName: ` + syncClaimName + `
  sync:
    direction: pull
`

// The objects of the Pod Security spec: a namespace that enforces the restricted
// level, with the same kind of Treebind as the sync workload spec.
const (
	restrictedNamespace = "treebind-restricted-e2e"
	restrictedName      = "e2e-restricted"
	restrictedPodName   = restrictedName + "-sync-0"
	restrictedKeyName   = restrictedName + "-key"
	restrictedKeyFile   = "id_ecdsa"

	// keyReaderPodName runs the pod template of the operator with an image that
	// has ssh-keygen, as a non-root user the image has a passwd entry for.
	keyReaderPodName = restrictedName + "-key-reader"
	keyReaderImage   = "alpine/git:v2.54.0"
	nonRootUID       = int64(65534)
)

const restrictedManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: ` + restrictedNamespace + `
  labels:
    pod-security.kubernetes.io/enforce: restricted
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ` + restrictedName + `-data
  namespace: ` + restrictedNamespace + `
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + restrictedName + `-hosts
  namespace: ` + restrictedNamespace + `
data:
  known_hosts: github.com ssh-ed25519 AAAA
---
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata:
  name: ` + restrictedName + `
  namespace: ` + restrictedNamespace + `
spec:
  repository:
    url: ssh://git@github.com/example/dashboards.git
    branch: main
    auth:
      sshKeySecretRef: {name: ` + restrictedKeyName + `, key: ` + restrictedKeyFile + `}
      knownHostsConfigMapRef: {name: ` + restrictedName + `-hosts, key: known_hosts}
  target:
    claimName: ` + restrictedName + `-data
  sync:
    direction: pull
`

// privilegedPodManifest is a pod the restricted level has to refuse.
const privilegedPodManifest = `apiVersion: v1
kind: Pod
metadata:
  name: privileged
  namespace: ` + restrictedNamespace + `
spec:
  containers:
  - name: privileged
    image: busybox
    securityContext:
      privileged: true
`

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching the logs of the controller manager pods")
			cmd := exec.Command("kubectl", "logs", "-l", controllerPodSelector, "-n", namespace, "--prefix")
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching the controller manager pod descriptions")
			cmd = exec.Command("kubectl", "describe", "pods", "-l", controllerPodSelector, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				podNames := controllerPods(g)
				g.Expect(podNames).To(HaveLen(controllerReplicas), "expected %d controller pods", controllerReplicas)
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the status of every pod")
				for _, podName := range podNames {
					cmd := exec.Command("kubectl", "get",
						"pods", podName, "-o", "jsonpath={.status.phase}",
						"-n", namespace,
					)
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
				}
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=treebind-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		It("should run two ready replicas, one of which holds the lease, and allow one disruption", func() {
			By("waiting for both replicas to be ready")
			Eventually(func(g Gomega) {
				g.Expect(readyControllerPods(g)).To(HaveLen(controllerReplicas))
			}).Should(Succeed())

			By("checking that one of them holds the lease, so a standby is ready without leading")
			Eventually(func(g Gomega) {
				g.Expect(readyControllerPods(g)).To(ContainElement(leaseHolderPod(g)))
			}).Should(Succeed())

			By("checking that the PodDisruptionBudget lets a drain evict one replica")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "poddisruptionbudget", podDisruptionBudgetName,
					"-n", namespace, "-o", "jsonpath={.status.disruptionsAllowed}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}).Should(Succeed())
		})

		// The pod is deleted without a grace period, so it cannot give the lease up and
		// the other replica has to wait for it to run out. The specs below create a
		// Treebind, and they only pass when the new leader reconciles it.
		It("should hand the lease to another replica when the leader is deleted", func() {
			var leader string
			Eventually(func(g Gomega) {
				leader = leaseHolderPod(g)
				g.Expect(leader).NotTo(BeEmpty())
			}).Should(Succeed())

			By("deleting the leader pod without a grace period")
			cmd := exec.Command("kubectl", "delete", "pod", leader, "-n", namespace, "--grace-period=0", "--force")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete the leader pod")

			By("waiting for a different pod to hold the lease")
			Eventually(func(g Gomega) {
				holder := leaseHolderPod(g)
				g.Expect(holder).NotTo(BeEmpty())
				g.Expect(holder).NotTo(Equal(leader))
			}).Should(Succeed())

			By("waiting for the Deployment to replace the deleted pod")
			Eventually(func(g Gomega) {
				g.Expect(readyControllerPods(g)).To(HaveLen(controllerReplicas))
			}).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})

	// The stub agent exits and its image cannot be pulled here, so this checks
	// that the StatefulSet exists and is owned, not that its pod is ready.
	Context("Sync workload", func() {
		AfterAll(func() {
			By("removing the Treebind, the marker pod, the claim and the credentials of the sync workload")
			cmd := exec.Command("kubectl", "delete", "treebind", syncTreebindName, "-n", syncNamespace,
				"--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pod", syncMarkerPodName, "-n", syncNamespace,
				"--ignore-not-found", "--now")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pvc", syncClaimName, "-n", syncNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "secret", syncKeySecretName, "-n", syncNamespace,
				"--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "configmap", syncHostsMapName, "-n", syncNamespace,
				"--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should own the sync StatefulSet and keep the claim and its data when the Treebind is deleted", func() {
			By("creating a claim and a Treebind")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(syncManifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the claim and the Treebind")

			cmd = exec.Command("kubectl", "get", "treebind", syncTreebindName, "-n", syncNamespace,
				"-o", "jsonpath={.metadata.uid}")
			treebindUID, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to read the Treebind")
			Expect(treebindUID).NotTo(BeEmpty())

			By("waiting for a one-replica StatefulSet that the Treebind owns")
			verifyStatefulSet := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", syncStatefulSetName, "-n", syncNamespace,
					"-o", "jsonpath={.spec.replicas} {.metadata.ownerReferences[0].kind} "+
						"{.metadata.ownerReferences[0].uid} {.metadata.ownerReferences[0].controller}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1 Treebind " + treebindUID + " true"))
			}
			Eventually(verifyStatefulSet).Should(Succeed())

			By("waiting for the ConfigMap of keys, which the Treebind owns and which holds the cluster's public keys")
			verifyKeys := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", syncKeysName, "-n", syncNamespace,
					"-o", "jsonpath={.metadata.ownerReferences[0].uid} {.metadata.ownerReferences[0].controller} "+
						"{.data.jwks\\.json}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				owner, keys, found := strings.Cut(strings.TrimSpace(output), " true ")
				g.Expect(found).To(BeTrue(), "the ConfigMap is not controlled by an owner: %s", output)
				g.Expect(owner).To(Equal(treebindUID))
				var set struct {
					Keys []struct {
						Kty string `json:"kty"`
						Kid string `json:"kid"`
					} `json:"keys"`
				}
				g.Expect(json.Unmarshal([]byte(keys), &set)).To(Succeed(), "the ConfigMap holds no key set: %s", keys)
				g.Expect(set.Keys).NotTo(BeEmpty())
				g.Expect(set.Keys[0].Kid).NotTo(BeEmpty())
			}
			Eventually(verifyKeys).Should(Succeed())

			By("writing a file to the claim from a second pod")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(syncMarkerManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the marker pod")

			verifyMarker := func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", syncMarkerPodName, "-n", syncNamespace, "--",
					"cat", "/data/marker")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal(syncMarkerContent))
			}
			Eventually(verifyMarker).Should(Succeed())

			By("deleting the Treebind")
			cmd = exec.Command("kubectl", "delete", "treebind", syncTreebindName, "-n", syncNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete the Treebind")

			By("waiting for the garbage collector to remove the StatefulSet")
			verifyStatefulSetGone := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", "-n", syncNamespace, "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(ContainSubstring(syncStatefulSetName))
			}
			Eventually(verifyStatefulSetGone).Should(Succeed())

			By("waiting for the garbage collector to remove the ConfigMap of keys as well")
			verifyKeysGone := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", "-n", syncNamespace, "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(ContainSubstring(syncKeysName))
			}
			Eventually(verifyKeysGone).Should(Succeed())

			By("checking that the claim is bound, has no owner, is not being deleted and still holds the file")
			// A claim that a pod uses stays in place while a deletion is pending, still
			// Bound, so only an empty deletionTimestamp shows that nobody asked for it.
			cmd = exec.Command("kubectl", "get", "pvc", syncClaimName, "-n", syncNamespace,
				"-o", "jsonpath={.status.phase}/{.metadata.ownerReferences}/{.metadata.deletionTimestamp}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "The claim is gone")
			Expect(output).To(Equal("Bound//"))
			Eventually(verifyMarker).Should(Succeed())

			By("asking the API server what the operator may delete")
			asOperator := fmt.Sprintf("--as=system:serviceaccount:%s:%s", namespace, serviceAccountName)
			cmd = exec.Command("kubectl", "auth", "can-i", "delete", "statefulsets", "-n", syncNamespace, asOperator)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "The check cannot tell what the operator may do")
			Expect(strings.TrimSpace(output)).To(Equal("yes"))
			cmd = exec.Command("kubectl", "auth", "can-i", "delete", "persistentvolumeclaims", "-n", syncNamespace,
				asOperator)
			output, _ = utils.Run(cmd)
			Expect(strings.TrimSpace(output)).To(Equal("no"))

			By("asking what the operator may do with ConfigMaps and Secrets: create and patch, and nothing else")
			for _, check := range []struct{ verb, resource, want string }{
				{"create", "configmaps", "yes"},
				{"patch", "configmaps", "yes"},
				{"get", "configmaps", "no"},
				{"list", "configmaps", "no"},
				{"watch", "configmaps", "no"},
				{"update", "configmaps", "no"},
				{"delete", "configmaps", "no"},
				{"get", "secrets", "no"},
				{"list", "secrets", "no"},
			} {
				cmd = exec.Command("kubectl", "auth", "can-i", check.verb, check.resource, "-n", syncNamespace, asOperator)
				output, _ = utils.Run(cmd)
				Expect(strings.TrimSpace(output)).To(Equal(check.want), "%s %s", check.verb, check.resource)
			}

			By("getting a token as the operator, for its own service account and for no other")
			const audience = "treebind.example.org/e2e/token"
			mint := func(account, accountNamespace string) (string, error) {
				cmd := exec.Command("kubectl", "create", "token", account, "-n", accountNamespace,
					"--audience="+audience, "--duration=10m", asOperator)
				return utils.Run(cmd)
			}
			token, err := mint(serviceAccountName, namespace)
			Expect(err).NotTo(HaveOccurred(), "The operator cannot get a token of its own service account")
			parts := strings.Split(strings.TrimSpace(token), ".")
			Expect(parts).To(HaveLen(3), "the answer is not a token")
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			Expect(err).NotTo(HaveOccurred())
			var claims struct {
				Subject  string   `json:"sub"`
				Audience []string `json:"aud"`
			}
			Expect(json.Unmarshal(payload, &claims)).To(Succeed())
			Expect(claims.Subject).To(Equal(fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccountName)))
			Expect(claims.Audience).To(Equal([]string{audience}))

			_, err = mint("default", namespace)
			Expect(err).To(MatchError(ContainSubstring("forbidden")),
				"the operator got a token of another service account of its namespace")
			_, err = mint("default", syncNamespace)
			Expect(err).To(MatchError(ContainSubstring("forbidden")),
				"the operator got a token of a service account of another namespace")
		})
	})

	// The agent image cannot be pulled here either, so the sync pod never starts.
	// It still only exists if the restricted level admitted it.
	Context("Pod Security", func() {
		BeforeAll(func() {
			By("creating a namespace that enforces the restricted level, with a Treebind in it")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(restrictedManifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the namespace and the Treebind")

			By("creating the Secret with a key that exists only for this test")
			cmd = exec.Command("kubectl", "create", "secret", "generic", restrictedKeyName, "-n", restrictedNamespace,
				"--from-literal="+restrictedKeyFile+"="+throwawayPrivateKey())
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the Secret")
		})

		// A pod that cannot start leaves the reason in the events and in the pod's
		// description of this namespace, not in the manager's.
		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			for _, args := range [][]string{
				{"get", "events", "--sort-by=.lastTimestamp"},
				{"describe", "pods"},
			} {
				cmd := exec.Command("kubectl", append(args, "-n", restrictedNamespace)...)
				output, err := utils.Run(cmd)
				if err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Failed to run kubectl %s: %s\n", strings.Join(args, " "), err)
					continue
				}
				_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n", strings.Join(args, " "), output)
			}
		})

		AfterAll(func() {
			By("removing the namespace that enforces the restricted level")
			cmd := exec.Command("kubectl", "delete", "ns", restrictedNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should admit the sync pod in a namespace that enforces the restricted level", func() {
			By("checking that the namespace refuses a privileged pod, so the level is really enforced")
			cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
			cmd.Stdin = strings.NewReader(privilegedPodManifest)
			output, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "A privileged pod was accepted")
			Expect(output).To(ContainSubstring(`violates PodSecurity "restricted:`))

			By("waiting for the StatefulSet to create its pod")
			verifyPod := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", restrictedPodName, "-n", restrictedNamespace,
					"-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(restrictedPodName))
			}
			Eventually(verifyPod).Should(Succeed())

			By("checking that no pod was refused")
			cmd = exec.Command("kubectl", "get", "events", "-n", restrictedNamespace,
				"--field-selector", "reason=FailedCreate", "-o", "jsonpath={.items[*].message}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to read the events")
			Expect(output).NotTo(ContainSubstring("PodSecurity"))
		})

		It("should let a non-root user read the SSH key the operator mounts", func() {
			By("taking the pod template of the sync StatefulSet")
			cmd := exec.Command("kubectl", "get", "statefulset", restrictedName+"-sync", "-n", restrictedNamespace,
				"-o", "json")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to read the StatefulSet")
			var statefulSet appsv1.StatefulSet
			Expect(json.Unmarshal([]byte(output), &statefulSet)).To(Succeed())

			By("running it as a non-root user, with an image that can load the key")
			spec := statefulSet.Spec.Template.Spec
			Expect(spec.SecurityContext).NotTo(BeNil(), "The pod template has no pod security context")
			spec.RestartPolicy = corev1.RestartPolicyNever
			uid := nonRootUID
			spec.SecurityContext.RunAsUser = &uid
			spec.SecurityContext.RunAsGroup = &uid
			// The pod needs the key and nothing else. Without the claim it cannot be
			// held up by it, and without the arguments the operator passes to the
			// agent, ssh-keygen does not receive them.
			spec.Volumes = slices.DeleteFunc(spec.Volumes, func(volume corev1.Volume) bool { return volume.Secret == nil })
			Expect(spec.Volumes).To(HaveLen(1), "The pod template has no Secret volume for the SSH key")
			keyVolume := spec.Volumes[0].Name
			spec.Containers[0].VolumeMounts = slices.DeleteFunc(spec.Containers[0].VolumeMounts,
				func(mount corev1.VolumeMount) bool { return mount.Name != keyVolume })
			spec.Containers[0].Args = nil
			spec.Containers[0].Image = keyReaderImage
			spec.Containers[0].Command = []string{"ssh-keygen", "-y", "-f", "/etc/treebind/ssh/" + restrictedKeyFile}
			pod := corev1.Pod{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
				ObjectMeta: metav1.ObjectMeta{Name: keyReaderPodName, Namespace: restrictedNamespace},
				Spec:       spec,
			}
			manifest, err := json.Marshal(pod)
			Expect(err).NotTo(HaveOccurred())
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(string(manifest))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the pod that loads the key")

			By("waiting for ssh-keygen to load the key")
			var phase string
			verifyDone := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", keyReaderPodName, "-n", restrictedNamespace,
					"-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				phase = output
				g.Expect(phase).To(Or(Equal(string(corev1.PodSucceeded)), Equal(string(corev1.PodFailed))))
			}
			Eventually(verifyDone).Should(Succeed())

			cmd = exec.Command("kubectl", "logs", keyReaderPodName, "-n", restrictedNamespace)
			logs, _ := utils.Run(cmd)
			Expect(phase).To(Equal(string(corev1.PodSucceeded)), "ssh-keygen could not load the key: %s", logs)
		})
	})
})

// throwawayPrivateKey returns a PEM encoded private key that is generated for one
// test run and never leaves it. It is generated, not committed, so that no private
// key sits in the repository.
func throwawayPrivateKey() string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	der, err := x509.MarshalECPrivateKey(key)
	Expect(err).NotTo(HaveOccurred())
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// controllerPods returns the names of the operator pods that are not being deleted.
func controllerPods(g Gomega) []string {
	cmd := exec.Command("kubectl", "get", "pods", "-l", controllerPodSelector, "-n", namespace,
		"-o", "go-template={{ range .items }}"+
			"{{ if not .metadata.deletionTimestamp }}"+
			"{{ .metadata.name }}"+
			"{{ \"\\n\" }}{{ end }}{{ end }}")
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve the controller-manager pods")
	return utils.GetNonEmptyLines(output)
}

// readyControllerPods returns the names of the operator pods that are ready and not
// being deleted. A standby is ready too, because readiness does not need the lease.
func readyControllerPods(g Gomega) []string {
	cmd := exec.Command("kubectl", "get", "pods", "-l", controllerPodSelector, "-n", namespace,
		"-o", "go-template={{ range .items }}{{ $name := .metadata.name }}"+
			"{{ if not .metadata.deletionTimestamp }}"+
			"{{ range .status.conditions }}{{ if and (eq .type \"Ready\") (eq .status \"True\") }}"+
			"{{ $name }}{{ \"\\n\" }}"+
			"{{ end }}{{ end }}"+
			"{{ end }}{{ end }}")
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve the ready controller-manager pods")
	return utils.GetNonEmptyLines(output)
}

// leaseHolderPod returns the name of the pod that holds the operator's Lease. The
// holder identity is the pod name, an underscore and a random suffix.
func leaseHolderPod(g Gomega) string {
	cmd := exec.Command("kubectl", "get", "lease", leaseName, "-n", namespace,
		"-o", "jsonpath={.spec.holderIdentity}")
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the Lease")
	pod, _, _ := strings.Cut(strings.TrimSpace(output), "_")
	return pod
}
