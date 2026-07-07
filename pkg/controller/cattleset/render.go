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
	"encoding/json"

	api "kubeops.dev/cattleset/apis/apps/v1"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// renderDeployment renders the local Deployment for a non-distributed CattleSet
// (and the base object each Member site's ManifestWork carries in distributed
// mode). The spec core is a literal field copy from the CattleSet, the mirror
// making it byte-identical to the input: adopting an existing same-spec
// Deployment by name produces no server-side-apply diff and restarts no pods.
func renderDeployment(cs *api.CattleSet) *apps.Deployment {
	return &apps.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cs.Name,
			Namespace: cs.Namespace,
			Labels:    renderedLabels(cs, nil),
		},
		Spec: cs.DeploymentSpecCore(),
	}
}

// renderService renders the herd's Service. When gated is true the Service
// starts unadmitted: the controller-owned admission term is added to the
// selector so no pod matches and the Service selects no endpoints, until the
// spoke gate agent removes it (design Section 6).
func renderService(cs *api.CattleSet, gated bool) *core.Service {
	if cs.Spec.Traffic == nil || cs.Spec.Traffic.ServiceName == "" {
		return nil
	}
	selector := map[string]string{}
	if cs.Spec.Selector != nil {
		for k, v := range cs.Spec.Selector.MatchLabels {
			selector[k] = v
		}
	}
	var annotations map[string]string
	if gated {
		selector[api.AdmittedSelectorKey] = api.AdmittedSelectorValue
		if cs.Spec.Gates != nil && len(cs.Spec.Gates.DataTiers) > 0 {
			if raw, err := json.Marshal(cs.Spec.Gates.DataTiers); err == nil {
				annotations = map[string]string{api.GatedDataTiersAnnotation: string(raw)}
			}
		}
	}
	return &core.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        cs.Spec.Traffic.ServiceName,
			Namespace:   cs.Namespace,
			Labels:      renderedLabels(cs, nil),
			Annotations: annotations,
		},
		Spec: core.ServiceSpec{
			Selector: selector,
			Ports: []core.ServicePort{{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromInt32(8080),
			}},
		},
	}
}

// renderedLabels stamps the CattleSet identity labels onto a rendered object,
// optionally recording the target cluster (distributed mode).
func renderedLabels(cs *api.CattleSet, cluster *string) map[string]string {
	out := map[string]string{
		api.CattleSetNameLabel:      cs.Name,
		api.CattleSetNamespaceLabel: cs.Namespace,
	}
	if cluster != nil {
		out[api.ClusterNameLabel] = *cluster
	}
	return out
}
