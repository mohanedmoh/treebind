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
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

var _ = Describe("Treebind Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		syncNamespacedName := types.NamespacedName{Name: resourceName + "-sync", Namespace: resourceNamespace}
		keysNamespacedName := types.NamespacedName{Name: resourceName + "-sync-keys", Namespace: resourceNamespace}
		treebind := &treebindv1alpha1.Treebind{}
		var controllerReconciler *TreebindReconciler

		reconcileOnce := func() {
			GinkgoHelper()
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
		}

		get := func() *treebindv1alpha1.Treebind {
			GinkgoHelper()
			got := &treebindv1alpha1.Treebind{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, got)).To(Succeed())
			return got
		}

		getStatefulSet := func() *appsv1.StatefulSet {
			GinkgoHelper()
			got := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, syncNamespacedName, got)).To(Succeed())
			return got
		}

		getService := func() *corev1.Service {
			GinkgoHelper()
			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, syncNamespacedName, got)).To(Succeed())
			return got
		}

		findCondition := func(conditionType string) *metav1.Condition {
			GinkgoHelper()
			condition := meta.FindStatusCondition(get().Status.Conditions, conditionType)
			Expect(condition).NotTo(BeNil())
			return condition
		}

		// reportAs plays the status poller: it writes the given conditions
		// through the status subresource, the way the reconciler never does.
		reportAs := func(conditions ...metav1.Condition) {
			GinkgoHelper()
			resource := get()
			for _, condition := range conditions {
				condition.ObservedGeneration = resource.Generation
				meta.SetStatusCondition(&resource.Status.Conditions, condition)
			}
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())
		}

		makeSyncPodReady := func() {
			GinkgoHelper()
			statefulSet := getStatefulSet()
			statefulSet.Status.Replicas = 1
			statefulSet.Status.ReadyReplicas = 1
			statefulSet.Status.ObservedGeneration = statefulSet.Generation
			Expect(k8sClient.Status().Update(ctx, statefulSet)).To(Succeed())
		}

		// breakTemplate makes the pod template invalid: the claim and the SSH key
		// would both be mounted at the same path, which the API server refuses.
		breakTemplate := func() {
			GinkgoHelper()
			resource := get()
			resource.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef()}
			resource.Spec.Target.MountPath = sshMountPath
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
		}

		reconcileFails := func() error {
			GinkgoHelper()
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).To(HaveOccurred())
			return err
		}

		BeforeEach(func() {
			controllerReconciler = &TreebindReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				AgentImage: testImage,

				StatusIssuer:  testIssuer,
				StatusSubject: testSubject,
				StatusKeys:    newTestPublisher(),
			}

			By("creating the custom resource for the Kind Treebind")
			err := k8sClient.Get(ctx, typeNamespacedName, treebind)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, newTreebind(resourceName))).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &treebindv1alpha1.Treebind{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Treebind")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Cleanup the sync objects, which envtest has no garbage collector to remove")
			sync := metav1.ObjectMeta{Name: syncNamespacedName.Name, Namespace: syncNamespacedName.Namespace}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: sync}))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: sync}))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: keysNamespacedName.Name, Namespace: keysNamespacedName.Namespace,
			}}))).To(Succeed())
		})

		It("reports a spec with only implemented fields as supported", func() {
			reconcileOnce()

			got := get()
			condition := meta.FindStatusCondition(got.Status.Conditions, treebindv1alpha1.ConditionSpecSupported)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(treebindv1alpha1.ReasonSupported))
			Expect(condition.ObservedGeneration).To(Equal(got.Generation))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		})

		It("reports the fields that are accepted but not implemented, and clears them once removed", func() {
			reconcileOnce()

			resource := get()
			resource.Spec.Placement.Mode = treebindv1alpha1.PlacementModeSidecar
			resource.Spec.Agent = &treebindv1alpha1.AgentSpec{NodeSelector: newAgentSpec().NodeSelector}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			reconcileOnce()

			got := get()
			condition := meta.FindStatusCondition(got.Status.Conditions, treebindv1alpha1.ConditionSpecSupported)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(treebindv1alpha1.ReasonNotImplemented))
			Expect(condition.Message).To(ContainSubstring(fieldNodeSelector))
			Expect(condition.Message).To(ContainSubstring(fieldPlacementMode))
			Expect(condition.ObservedGeneration).To(Equal(got.Generation))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			Expect(got.Generation).To(BeNumerically(">", int64(1)))

			got.Spec.Placement.Mode = treebindv1alpha1.PlacementModeStandalone
			got.Spec.Agent = nil
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			reconcileOnce()

			got = get()
			condition = meta.FindStatusCondition(got.Status.Conditions, treebindv1alpha1.ConditionSpecSupported)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(treebindv1alpha1.ReasonSupported))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		})

		It("leaves the object untouched when nothing changed since the last reconcile", func() {
			resource := get()
			resource.Spec.Agent = &treebindv1alpha1.AgentSpec{PriorityClassName: newAgentSpec().PriorityClassName}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			reconcileOnce()
			first := get()

			reconcileOnce()
			second := get()

			Expect(second.ResourceVersion).To(Equal(first.ResourceVersion))
		})

		It("creates a headless Service and a one-replica StatefulSet that the Treebind owns", func() {
			reconcileOnce()

			owner := get()
			statefulSet, service := getStatefulSet(), getService()

			wantSelector, wantLabels := wantPodLabels(resourceName), wantObjectLabels(resourceName)

			for _, object := range []client.Object{statefulSet, service} {
				ref := metav1.GetControllerOf(object)
				Expect(ref).NotTo(BeNil())
				Expect(ref.APIVersion).To(Equal(treebindv1alpha1.GroupVersion.String()))
				Expect(ref.Kind).To(Equal("Treebind"))
				Expect(ref.Name).To(Equal(resourceName))
				Expect(ref.UID).To(Equal(owner.UID))
				Expect(ref.BlockOwnerDeletion).To(HaveValue(BeTrue()))
				Expect(object.GetLabels()).To(Equal(wantLabels))
			}

			Expect(service.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
			Expect(service.Spec.Selector).To(Equal(wantSelector))
			Expect(service.Spec.Ports).To(BeEmpty())

			Expect(statefulSet.Spec.Replicas).To(HaveValue(Equal(int32(1))))
			Expect(statefulSet.Spec.ServiceName).To(Equal(syncNamespacedName.Name))
			Expect(statefulSet.Spec.Selector.MatchLabels).To(Equal(wantSelector))
			Expect(statefulSet.Spec.PodManagementPolicy).To(Equal(appsv1.ParallelPodManagement))
			Expect(statefulSet.Spec.UpdateStrategy.Type).To(Equal(appsv1.RollingUpdateStatefulSetStrategyType))
			Expect(statefulSet.Spec.VolumeClaimTemplates).To(BeEmpty())
			Expect(statefulSet.Annotations).To(HaveKey(templateHashAnnotation))
			Expect(statefulSet.Spec.Template.Labels).To(Equal(wantLabels))

			pod := statefulSet.Spec.Template.Spec
			Expect(pod.Containers).To(HaveLen(1))
			Expect(pod.Containers[0].Image).To(Equal(testImage))
			Expect(pod.Containers[0].VolumeMounts).To(ConsistOf(
				corev1.VolumeMount{Name: dataVolumeName, MountPath: defaultMountPath},
				corev1.VolumeMount{Name: statusKeysVolumeName, MountPath: statusKeysMountPath, ReadOnly: true}))
			Expect(pod.Volumes).To(HaveLen(2))
			Expect(pod.Volumes[0].Name).To(Equal(dataVolumeName))
			Expect(pod.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(owner.Spec.Target.ClaimName))
			Expect(pod.Volumes[1].ConfigMap.Name).To(Equal(owner.Name + "-sync-keys"))
			Expect(pod.Volumes[1].ConfigMap.Optional).To(HaveValue(BeTrue()))
			Expect(pod.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		})

		It("mounts the claim at the requested path and the credentials the spec names", func() {
			resource := get()
			resource.Spec.Target.MountPath = testCustomPath
			resource.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{
				SSHKeySecretRef:        sshKeyRef(),
				KnownHostsConfigMapRef: knownHostsRef(),
			}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			reconcileOnce()

			pod := getStatefulSet().Spec.Template.Spec
			Expect(pod.Containers[0].VolumeMounts).To(ConsistOf(
				corev1.VolumeMount{Name: dataVolumeName, MountPath: testCustomPath},
				corev1.VolumeMount{Name: statusKeysVolumeName, MountPath: statusKeysMountPath, ReadOnly: true},
				corev1.VolumeMount{Name: sshVolumeName, MountPath: sshMountPath, ReadOnly: true},
				corev1.VolumeMount{Name: knownHostsVolumeName, MountPath: knownHostsMountPath, ReadOnly: true},
			))
			Expect(pod.Volumes).To(HaveLen(4))
			secret := pod.Volumes[2].Secret
			Expect(secret.SecretName).To(Equal(testSSHKeyName))
			Expect(secret.Items).To(ConsistOf(corev1.KeyToPath{Key: testSSHKeyKey, Path: testSSHKeyKey}))
			Expect(secret.DefaultMode).To(HaveValue(Equal(int32(0o444))))
			configMap := pod.Volumes[3].ConfigMap
			Expect(configMap.Name).To(Equal(testHostsName))
			Expect(configMap.Items).To(ConsistOf(corev1.KeyToPath{Key: testHostsKey, Path: testHostsKey}))
		})

		It("writes nothing when a second reconcile has nothing to change", func() {
			reconcileOnce()
			firstTreebind, firstStatefulSet, firstService := get(), getStatefulSet(), getService()

			reconcileOnce()

			Expect(get().ResourceVersion).To(Equal(firstTreebind.ResourceVersion))
			Expect(getStatefulSet().ResourceVersion).To(Equal(firstStatefulSet.ResourceVersion))
			Expect(getService().ResourceVersion).To(Equal(firstService.ResourceVersion))
		})

		It("restores one replica when the StatefulSet is scaled", func() {
			reconcileOnce()

			statefulSet := getStatefulSet()
			replicas := int32(2)
			statefulSet.Spec.Replicas = &replicas
			Expect(k8sClient.Update(ctx, statefulSet)).To(Succeed())

			reconcileOnce()

			Expect(getStatefulSet().Spec.Replicas).To(HaveValue(Equal(int32(1))))
		})

		It("rewrites the pod template when the mount path or the credentials change, and only then", func() {
			reconcileOnce()
			hash := getStatefulSet().Annotations[templateHashAnnotation]

			resource := get()
			resource.Spec.Target.MountPath = testCustomPath
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			reconcileOnce()

			statefulSet := getStatefulSet()
			Expect(statefulSet.Annotations[templateHashAnnotation]).NotTo(Equal(hash))
			Expect(statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath).To(Equal(testCustomPath))
			hash = statefulSet.Annotations[templateHashAnnotation]

			resource = get()
			resource.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef()}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			reconcileOnce()

			statefulSet = getStatefulSet()
			Expect(statefulSet.Annotations[templateHashAnnotation]).NotTo(Equal(hash))
			Expect(statefulSet.Spec.Template.Spec.Volumes).To(HaveLen(3))
			hash = statefulSet.Annotations[templateHashAnnotation]

			resource = get()
			resource.Spec.Repository.Branch = "release"
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			reconcileOnce()

			Expect(getStatefulSet().Annotations[templateHashAnnotation]).To(Equal(hash))
		})

		It("reports Ready only once the StatefulSet has a ready pod for the current template", func() {
			reconcileOnce()

			condition := meta.FindStatusCondition(get().Status.Conditions, treebindv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(treebindv1alpha1.ReasonSyncPodNotReady))
			Expect(condition.Message).To(Equal("0 of 1 sync pods ready"))

			statefulSet := getStatefulSet()
			statefulSet.Status.Replicas = 1
			statefulSet.Status.ReadyReplicas = 1
			statefulSet.Status.ObservedGeneration = statefulSet.Generation - 1
			Expect(k8sClient.Status().Update(ctx, statefulSet)).To(Succeed())
			reconcileOnce()

			condition = meta.FindStatusCondition(get().Status.Conditions, treebindv1alpha1.ConditionReady)
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))

			statefulSet = getStatefulSet()
			statefulSet.Status.ObservedGeneration = statefulSet.Generation
			Expect(k8sClient.Status().Update(ctx, statefulSet)).To(Succeed())
			reconcileOnce()

			got := get()
			condition = meta.FindStatusCondition(got.Status.Conditions, treebindv1alpha1.ConditionReady)
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(treebindv1alpha1.ReasonSyncPodReady))
			Expect(condition.Message).To(Equal("1 of 1 sync pods ready"))
			Expect(condition.ObservedGeneration).To(Equal(got.Generation))
		})

		It("reports a failed reconcile in Ready, publishes the rest of the status and returns the error", func() {
			breakTemplate()

			Expect(reconcileFails()).To(MatchError(ContainSubstring("must be unique")))

			got := get()
			ready := meta.FindStatusCondition(got.Status.Conditions, treebindv1alpha1.ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonReconcileFailed))
			Expect(ready.Message).To(And(
				HavePrefix("The last reconcile failed: ensure StatefulSet default/"+resourceName+"-sync"),
				ContainSubstring("must be unique"),
			))
			Expect(ready.ObservedGeneration).To(Equal(got.Generation))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			Expect(findCondition(treebindv1alpha1.ConditionSpecSupported).Status).To(Equal(metav1.ConditionTrue))
			Expect(findCondition(treebindv1alpha1.ConditionSynced).Status).To(Equal(metav1.ConditionUnknown))
			Expect(findCondition(treebindv1alpha1.ConditionConflict).Status).To(Equal(metav1.ConditionUnknown))
			Expect(findCondition(treebindv1alpha1.ConditionAgentUnreachable).Status).To(Equal(metav1.ConditionUnknown))
		})

		It("leaves AgentUnreachable to the status poller once it exists, and it does not change Ready", func() {
			reconcileOnce()
			makeSyncPodReady()
			reportAs(metav1.Condition{
				Type:    treebindv1alpha1.ConditionAgentUnreachable,
				Status:  metav1.ConditionTrue,
				Reason:  treebindv1alpha1.ReasonConnectionFailed,
				Message: "The operator cannot reach the sync agent",
			})
			before := findCondition(treebindv1alpha1.ConditionAgentUnreachable)

			reconcileOnce()

			Expect(findCondition(treebindv1alpha1.ConditionAgentUnreachable)).To(Equal(before))
			ready := findCondition(treebindv1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionTrue), "an agent that cannot be read does not make the Treebind not ready")
			Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonSyncPodReady))
		})

		It("writes nothing when the same failure repeats", func() {
			breakTemplate()
			Expect(reconcileFails()).To(HaveOccurred())
			first := get()

			Expect(reconcileFails()).To(HaveOccurred())

			Expect(get().ResourceVersion).To(Equal(first.ResourceVersion))
		})

		It("goes back to the computed Ready once the sync objects can be written again", func() {
			breakTemplate()
			Expect(reconcileFails()).To(HaveOccurred())

			resource := get()
			resource.Spec.Target.MountPath = testCustomPath
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			reconcileOnce()

			ready := findCondition(treebindv1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonSyncPodNotReady))
			Expect(ready.Message).To(Equal(messageNoPodReady))
			Expect(ready.ObservedGeneration).To(Equal(get().Generation))
		})

		// reconcilerWith returns a reconciler whose client runs the given functions
		// instead of the real client's.
		reconcilerWith := func(funcs interceptor.Funcs) *TreebindReconciler {
			GinkgoHelper()
			watching, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			return &TreebindReconciler{
				Client:     interceptor.NewClient(watching, funcs),
				Scheme:     k8sClient.Scheme(),
				AgentImage: testImage,

				StatusIssuer:  testIssuer,
				StatusSubject: testSubject,
				StatusKeys:    newTestPublisher(),
			}
		}

		// failCreateOf returns functions that fail the creation of one kind of
		// object with the given error and leave everything else alone.
		failCreateOf := func(kind client.Object, failure func(ctx context.Context) error) interceptor.Funcs {
			return interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if reflect.TypeOf(obj) == reflect.TypeOf(kind) {
						return failure(ctx)
					}
					return c.Create(ctx, obj, opts...)
				},
			}
		}

		reconcileWith := func(reconciler *TreebindReconciler, reconcileCtx context.Context) error {
			GinkgoHelper()
			_, err := reconciler.Reconcile(reconcileCtx, reconcile.Request{NamespacedName: typeNamespacedName})
			return err
		}

		DescribeTable("does not report an error that a retry makes go away",
			func(failure func() error, matches func(error) bool) {
				before := get()

				err := reconcileWith(reconcilerWith(failCreateOf(&appsv1.StatefulSet{},
					func(context.Context) error { return failure() })), ctx)

				Expect(err).To(HaveOccurred())
				Expect(matches(err)).To(BeTrue(), "the error was changed: %v", err)
				after := get()
				Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
				Expect(after.Status.Conditions).To(BeEmpty())
			},
			Entry("an update conflict", func() error {
				return errors.NewConflict(appsv1.Resource("statefulsets"), "x", fmt.Errorf("the cache is stale"))
			}, errors.IsConflict),
			Entry("an object that already exists", func() error {
				return errors.NewAlreadyExists(appsv1.Resource("statefulsets"), "x")
			}, errors.IsAlreadyExists),
		)

		It("does not report the cancellation of the context when the manager stops", func() {
			cancellable, cancel := context.WithCancel(ctx)
			defer cancel()
			before := get()

			err := reconcileWith(reconcilerWith(failCreateOf(&appsv1.StatefulSet{}, func(context.Context) error {
				cancel()
				return context.Canceled
			})), cancellable)

			Expect(err).To(MatchError(context.Canceled))
			Expect(err.Error()).NotTo(ContainSubstring("update status"))
			Expect(get().ResourceVersion).To(Equal(before.ResourceVersion))
		})

		It("reports a failure to create the Service and creates no StatefulSet", func() {
			err := reconcileWith(reconcilerWith(failCreateOf(&corev1.Service{}, func(context.Context) error {
				return errors.NewInternalError(fmt.Errorf("etcd is slow"))
			})), ctx)

			Expect(err).To(MatchError(ContainSubstring("etcd is slow")))
			ready := findCondition(treebindv1alpha1.ConditionReady)
			Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonReconcileFailed))
			Expect(ready.Message).To(HavePrefix("The last reconcile failed: ensure Service default/" + resourceName + "-sync"))
			Expect(errors.IsNotFound(k8sClient.Get(ctx, syncNamespacedName, &appsv1.StatefulSet{}))).To(BeTrue())
		})

		It("returns both errors when the status cannot be written either", func() {
			funcs := failCreateOf(&appsv1.StatefulSet{}, func(context.Context) error {
				return errors.NewInternalError(fmt.Errorf("etcd is slow"))
			})
			funcs.SubResourceUpdate = func(context.Context, client.Client, string, client.Object,
				...client.SubResourceUpdateOption,
			) error {
				return errors.NewConflict(treebindv1alpha1.GroupVersion.WithResource("treebinds").GroupResource(),
					resourceName, fmt.Errorf("changed meanwhile"))
			}
			before := get()

			err := reconcileWith(reconcilerWith(funcs), ctx)

			Expect(err).To(MatchError(ContainSubstring("etcd is slow")))
			Expect(err).To(MatchError(ContainSubstring("update status of Treebind")))
			Expect(err).To(MatchError(ContainSubstring("changed meanwhile")))
			Expect(get().ResourceVersion).To(Equal(before.ResourceVersion))
		})

		getKeysConfigMap := func() *corev1.ConfigMap {
			GinkgoHelper()
			got := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, keysNamespacedName, got)).To(Succeed())
			return got
		}

		// failingKeys returns a publisher that cannot read the cluster's keys.
		failingKeys := func() *StatusKeyPublisher {
			source := &staticKeys{}
			source.set(nil, fmt.Errorf("the API server said no"))
			return &StatusKeyPublisher{Source: source}
		}

		It("puts the cluster's keys in a ConfigMap that the Treebind owns", func() {
			reconcileOnce()

			configMap := getKeysConfigMap()
			Expect(configMap.Data).To(Equal(map[string]string{statusKeysFile: string(testKeySet())}))
			ref := metav1.GetControllerOf(configMap)
			Expect(ref).NotTo(BeNil())
			Expect(ref.UID).To(Equal(get().UID))
		})

		It("creates the ConfigMap of keys before the StatefulSet, so that a new pod has the file when it starts", func() {
			var created []string
			reconciler := reconcilerWith(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					created = append(created, reflect.TypeOf(obj).Elem().Name())
					return c.Create(ctx, obj, opts...)
				},
			})

			Expect(reconcileWith(reconciler, ctx)).To(Succeed())

			Expect(created).To(Equal([]string{"ConfigMap", "Service", "StatefulSet"}))
		})

		// retryDelayIsShort checks the delay a pass asks for after the keys failed.
		retryDelayIsShort := func(result reconcile.Result) {
			GinkgoHelper()
			Expect(result.RequeueAfter).To(BeNumerically(">=", statusKeysRetry*4/5))
			Expect(result.RequeueAfter).To(BeNumerically("<=", statusKeysRetry*6/5))
		}

		It("creates the Service and the StatefulSet when the keys cannot be read, leaves Ready alone and tries again soon", func() {
			reconciler := reconcilerWith(interceptor.Funcs{})
			reconciler.StatusKeys = failingKeys()

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

			Expect(err).NotTo(HaveOccurred())
			retryDelayIsShort(result)
			ready := findCondition(treebindv1alpha1.ConditionReady)
			Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonSyncPodNotReady), "the keys are not what Ready is about")
			Expect(ready.Message).To(Equal(messageNoPodReady))
			Expect(getService()).NotTo(BeNil())
			Expect(getStatefulSet()).NotTo(BeNil())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, keysNamespacedName, &corev1.ConfigMap{}))).To(BeTrue())
		})

		// logged runs the reconcile with a logger that keeps what it is told, and returns
		// the lines that report an error.
		logged := func(reconciler *TreebindReconciler) string {
			GinkgoHelper()
			var output bytes.Buffer
			logger := zap.New(zap.WriteTo(&output))
			_, err := reconciler.Reconcile(logf.IntoContext(ctx, logger), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			var errorLines []string
			for line := range strings.SplitSeq(output.String(), "\n") {
				if strings.Contains(line, `"level":"error"`) {
					errorLines = append(errorLines, line)
				}
			}
			return strings.Join(errorLines, "\n")
		}

		It("does not log a failed read of the keys as an error for every Treebind, because the key source has", func() {
			reconciler := reconcilerWith(interceptor.Funcs{})
			reconciler.StatusKeys = failingKeys()

			Expect(logged(reconciler)).To(BeEmpty())
		})

		It("logs a ConfigMap of that name that it did not create as an error, because only this Treebind has it", func() {
			Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: keysNamespacedName.Name, Namespace: keysNamespacedName.Namespace},
				Data:       map[string]string{statusKeysFile: foreignData},
			})).To(Succeed())

			errorLines := logged(controllerReconciler)

			Expect(errorLines).To(ContainSubstring("Could not put the cluster's public signing keys"))
			Expect(errorLines).To(ContainSubstring("first owner is this Treebind"))
		})

		It("keeps Ready True for a sync pod that is ready while the keys cannot be read", func() {
			reconcileOnce()
			makeSyncPodReady()
			reconciler := reconcilerWith(interceptor.Funcs{})
			reconciler.StatusKeys = failingKeys()

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

			Expect(err).NotTo(HaveOccurred())
			ready := findCondition(treebindv1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonSyncPodReady))
		})

		It("leaves a ConfigMap of that name that it did not create alone, still creates the rest and leaves Ready alone", func() {
			Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: keysNamespacedName.Name, Namespace: keysNamespacedName.Namespace},
				Data:       map[string]string{statusKeysFile: foreignData},
			})).To(Succeed())

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

			Expect(err).NotTo(HaveOccurred())
			retryDelayIsShort(result)
			Expect(findCondition(treebindv1alpha1.ConditionReady).Reason).To(Equal(treebindv1alpha1.ReasonSyncPodNotReady))
			Expect(getKeysConfigMap().Data).To(Equal(map[string]string{statusKeysFile: foreignData}))
			Expect(getStatefulSet()).NotTo(BeNil())
		})

		It("does not ask for the Treebinds whose keys failed at the same moment", func() {
			controllerReconciler.StatusKeys = failingKeys()
			delays := map[time.Duration]bool{}
			for range 30 {
				result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())
				retryDelayIsShort(result)
				delays[result.RequeueAfter] = true
			}

			Expect(len(delays)).To(BeNumerically(">", 1), "the delay is the same every time: %v", delays)
		})

		It("reports only the failure of the Service in Ready when the keys fail as well", func() {
			reconciler := reconcilerWith(failCreateOf(&corev1.Service{}, func(context.Context) error {
				return errors.NewInternalError(fmt.Errorf("etcd is slow"))
			}))
			reconciler.StatusKeys = failingKeys()

			err := reconcileWith(reconciler, ctx)

			Expect(err).To(MatchError(ContainSubstring("etcd is slow")))
			Expect(err).NotTo(MatchError(ContainSubstring("the API server said no")))
			ready := findCondition(treebindv1alpha1.ConditionReady)
			Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonReconcileFailed))
			Expect(ready.Message).To(HavePrefix("The last reconcile failed: ensure Service default/" + resourceName + "-sync"))
			Expect(ready.Message).NotTo(ContainSubstring("the API server said no"))
		})

		It("makes the Service publish the name of a pod that is not ready, also on a Service made without it", func() {
			reconcileOnce()
			service := getService()
			Expect(service.Spec.PublishNotReadyAddresses).To(BeTrue())

			service.Spec.PublishNotReadyAddresses = false
			Expect(k8sClient.Update(ctx, service)).To(Succeed())
			reconcileOnce()

			Expect(getService().Spec.PublishNotReadyAddresses).To(BeTrue())
		})

		It("asks to be reconciled again after about five minutes, to look at the keys again", func() {
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">=", statusKeysRefresh*4/5))
			Expect(result.RequeueAfter).To(BeNumerically("<=", statusKeysRefresh*6/5))
		})

		It("does not ask for every Treebind at the same moment", func() {
			delays := map[time.Duration]bool{}
			for range 30 {
				result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())
				delays[result.RequeueAfter] = true
			}

			Expect(len(delays)).To(BeNumerically(">", 1), "the delay is the same every time: %v", delays)
		})

		It("leaves the delay to the error when the sync objects cannot be written", func() {
			reconciler := reconcilerWith(failCreateOf(&corev1.Service{}, func(context.Context) error {
				return errors.NewInternalError(fmt.Errorf("etcd is slow"))
			}))
			reconciler.StatusKeys = failingKeys()

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

			Expect(err).To(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("adds Synced, Conflict and AgentUnreachable as Unknown until the sync agent has reported", func() {
			reconcileOnce()

			generation := get().Generation
			for _, conditionType := range []string{
				treebindv1alpha1.ConditionSynced,
				treebindv1alpha1.ConditionConflict,
				treebindv1alpha1.ConditionAgentUnreachable,
			} {
				condition := findCondition(conditionType)
				Expect(condition.Status).To(Equal(metav1.ConditionUnknown), conditionType)
				Expect(condition.Reason).To(Equal(treebindv1alpha1.ReasonAwaitingAgentReport), conditionType)
				Expect(condition.Message).To(Equal(wantAwaitingMessage), conditionType)
				Expect(condition.ObservedGeneration).To(Equal(generation), conditionType)
			}
		})

		It("leaves Synced and Conflict to the status poller once they exist", func() {
			reconcileOnce()

			reportAs(
				metav1.Condition{
					Type:   treebindv1alpha1.ConditionSynced,
					Status: metav1.ConditionTrue,
					Reason: reasonInSync,
				},
				metav1.Condition{
					Type:   treebindv1alpha1.ConditionConflict,
					Status: metav1.ConditionTrue,
					Reason: reasonConflictParked,
				},
			)
			reported := get()
			syncedBefore := findCondition(treebindv1alpha1.ConditionSynced)
			conflictBefore := findCondition(treebindv1alpha1.ConditionConflict)

			reconcileOnce()

			Expect(findCondition(treebindv1alpha1.ConditionSynced)).To(Equal(syncedBefore))
			Expect(findCondition(treebindv1alpha1.ConditionConflict)).To(Equal(conflictBefore))
			Expect(get().ResourceVersion).To(Equal(reported.ResourceVersion))

			By("changing the spec, which moves the generation the reconciler writes to its own conditions")
			resource := get()
			resource.Spec.Repository.Branch = "staging"
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			reconcileOnce()

			got := get()
			Expect(got.Generation).To(BeNumerically(">", reported.Generation))
			Expect(findCondition(treebindv1alpha1.ConditionReady).ObservedGeneration).To(Equal(got.Generation))
			Expect(findCondition(treebindv1alpha1.ConditionSynced)).To(Equal(syncedBefore))
			Expect(findCondition(treebindv1alpha1.ConditionConflict)).To(Equal(conflictBefore))
		})

		It("keeps Ready True while a conflict waits, and turns it False when Synced is False", func() {
			reconcileOnce()
			makeSyncPodReady()
			reportAs(
				metav1.Condition{
					Type:   treebindv1alpha1.ConditionSynced,
					Status: metav1.ConditionTrue,
					Reason: reasonInSync,
				},
				metav1.Condition{
					Type:   treebindv1alpha1.ConditionConflict,
					Status: metav1.ConditionTrue,
					Reason: reasonConflictParked,
				},
			)
			reconcileOnce()

			ready := findCondition(treebindv1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonSyncPodReady))
			Expect(ready.Message).To(Equal(messagePodReady))

			By("reporting a failed sync while the pod is still ready")
			reportAs(metav1.Condition{
				Type:    treebindv1alpha1.ConditionSynced,
				Status:  metav1.ConditionFalse,
				Reason:  reasonSyncFailed,
				Message: "the last sync failed",
			})
			reconcileOnce()

			got := get()
			ready = findCondition(treebindv1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonNotSynced))
			Expect(ready.Message).To(Equal("1 of 1 sync pods ready, but Synced is False: SyncFailed"))
			Expect(ready.ObservedGeneration).To(Equal(got.Generation))
			Expect(findCondition(treebindv1alpha1.ConditionConflict).Status).To(Equal(metav1.ConditionTrue))

			By("reconciling again with nothing changed")
			reconcileOnce()
			Expect(get().ResourceVersion).To(Equal(got.ResourceVersion))

			By("reporting a good sync again")
			reportAs(metav1.Condition{
				Type:   treebindv1alpha1.ConditionSynced,
				Status: metav1.ConditionTrue,
				Reason: reasonInSync,
			})
			reconcileOnce()

			Expect(findCondition(treebindv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		})

		It("does not recreate the sync objects of a Treebind that is being deleted", func() {
			const holdFinalizer = "treebind.example.org/test-hold"
			reconcileOnce()

			held := get()
			held.Finalizers = append(held.Finalizers, holdFinalizer)
			Expect(k8sClient.Update(ctx, held)).To(Succeed())
			DeferCleanup(func() {
				got := get()
				got.Finalizers = nil
				Expect(k8sClient.Update(ctx, got)).To(Succeed())
			})

			By("deleting the Treebind and the sync objects the way the garbage collector would")
			Expect(k8sClient.Delete(ctx, get())).To(Succeed())
			Expect(get().DeletionTimestamp.IsZero()).To(BeFalse())
			Expect(k8sClient.Delete(ctx, getStatefulSet())).To(Succeed())
			Expect(k8sClient.Delete(ctx, getService())).To(Succeed())

			reconcileOnce()

			Expect(errors.IsNotFound(k8sClient.Get(ctx, syncNamespacedName, &appsv1.StatefulSet{}))).To(BeTrue())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, syncNamespacedName, &corev1.Service{}))).To(BeTrue())
		})

		It("does nothing for a Treebind that no longer exists", func() {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "gone", Namespace: resourceNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
