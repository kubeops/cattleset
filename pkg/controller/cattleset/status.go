/*
Copyright AppsCode Inc. and Contributors.

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

package cattleset

import (
	api "kubeops.dev/cattleset/apis/apps/v1"

	apps "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// projectLocalStatus mirrors an owned Deployment's status aggregates into the
// CattleSet status, and publishes status.selector for the scale subresource so
// an HPA attached to the CattleSet matches the herd's pods. This is the
// non-distributed projection (one Deployment, one set of aggregates).
func projectLocalStatus(cs *api.CattleSet, dep *apps.Deployment) {
	cs.Status.ObservedGeneration = cs.Generation
	cs.Status.Selector = metav1.FormatLabelSelector(cs.Spec.Selector)

	st := dep.Status
	cs.Status.Replicas = st.Replicas
	cs.Status.UpdatedReplicas = st.UpdatedReplicas
	cs.Status.ReadyReplicas = st.ReadyReplicas
	cs.Status.AvailableReplicas = st.AvailableReplicas
	cs.Status.UnavailableReplicas = st.UnavailableReplicas
	cs.Status.CollisionCount = st.CollisionCount

	setConditionFromDeployment(cs, dep, apps.DeploymentAvailable, api.ConditionAvailable)
	setConditionFromDeployment(cs, dep, apps.DeploymentProgressing, api.ConditionProgressing)
}

// setConditionFromDeployment copies the named Deployment condition onto the
// CattleSet under the CattleSet condition name.
func setConditionFromDeployment(cs *api.CattleSet, dep *apps.Deployment, depType apps.DeploymentConditionType, csType string) {
	var found *apps.DeploymentCondition
	for j := range dep.Status.Conditions {
		if dep.Status.Conditions[j].Type == depType {
			found = &dep.Status.Conditions[j]
		}
	}
	if found == nil {
		return
	}
	meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{
		Type:               csType,
		Status:             metav1.ConditionStatus(found.Status),
		Reason:             nonEmpty(found.Reason, "Reconciled"),
		Message:            found.Message,
		ObservedGeneration: cs.Generation,
	})
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
