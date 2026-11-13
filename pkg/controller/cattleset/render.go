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
	"kubeops.dev/cattleset/pkg/controller/gate"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// renderService renders the herd's Service. A non-nil gate makes the Service
// start unadmitted: the controller-owned admission term is added to the selector
// so no pod matches and the Service selects no endpoints, and the gate spec is
// stored in the annotation the spoke gate agent reads. The agent is then the
// selector's only writer. A nil gate renders an ordinary Service.
func renderService(cs *api.CattleSet, spec *gate.GateSpec) (*core.Service, error) {
	if !cs.HasTraffic() {
		return nil, nil
	}
	selector := map[string]string{}
	if cs.Spec.Selector != nil {
		for k, v := range cs.Spec.Selector.MatchLabels {
			selector[k] = v
		}
	}
	var annotations map[string]string
	if spec != nil {
		selector[api.AdmittedSelectorKey] = api.AdmittedSelectorValue
		raw, err := spec.Encode()
		if err != nil {
			return nil, err
		}
		annotations = map[string]string{api.GateAnnotation: raw}
	}
	ports := make([]core.ServicePort, len(cs.Spec.Traffic.Ports))
	for i := range cs.Spec.Traffic.Ports {
		cs.Spec.Traffic.Ports[i].DeepCopyInto(&ports[i])
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
			Ports:    ports,
		},
	}, nil
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
