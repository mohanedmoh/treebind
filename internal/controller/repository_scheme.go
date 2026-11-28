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
	"net/url"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// The CRD admits http:// repository addresses and the agent refuses them at start (docs/adr/0018), so a sync pod made for
// one exits for ever, and the only sign that a person got was AgentUnreachable saying ConnectionFailed after three
// minutes. The reconcile makes no sync pod for such a Treebind and says why in Ready, from its first pass.
//
// Whether plain http gets supported is open (#268). If it does, or if the CRD stops admitting it, delete this file and its
// test, and the use of usesPlainHTTP in Reconcile.

// usesPlainHTTP says whether the address of the repository has the scheme http. It reads the scheme as the agent does,
// which takes it in any case.
func usesPlainHTTP(address string) bool {
	parsed, err := url.Parse(address)
	return err == nil && parsed.Scheme == "http"
}

// unsupportedRepositorySchemeCondition builds the Ready condition of a Treebind whose repository has the scheme http.
// The message does not hold the address, which can name a host that the reader should not see.
func unsupportedRepositorySchemeCondition(generation int64) metav1.Condition {
	return metav1.Condition{
		Type:   treebindv1alpha1.ConditionReady,
		Status: metav1.ConditionFalse,
		Reason: treebindv1alpha1.ReasonUnsupportedRepositoryScheme,
		Message: "http:// repositories are not supported: use an ssh:// or https:// address in spec.repository.url " +
			"(see docs/checkout.md). No sync pod is created or changed until then",
		ObservedGeneration: generation,
	}
}

// unsupportedAgentUserCondition is Ready for a Treebind whose spec.agent.runAsUser is 0. The sync pod keeps runAsNonRoot,
// which the kubelet holds against user 0, so the operator makes no pod of it and leaves the one there was as it is
// (docs/adr/0044).
func unsupportedAgentUserCondition(generation int64) metav1.Condition {
	return metav1.Condition{
		Type:   treebindv1alpha1.ConditionReady,
		Status: metav1.ConditionFalse,
		Reason: treebindv1alpha1.ReasonUnsupportedAgentUser,
		Message: "spec.agent.runAsUser is 0, and the sync pod does not run as root: it keeps runAsNonRoot. Set another " +
			"user, or leave runAsUser out (see docs/pod-security.md). No sync pod is created or changed until then",
		ObservedGeneration: generation,
	}
}
