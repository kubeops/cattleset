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
	"fmt"

	api "kubeops.dev/cattleset/apis/apps/v1"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// manifestWorkName is the per-CattleSet, per-cluster ManifestWork name. Its
// namespace is the OCM managed-cluster name (the site).
func manifestWorkName(cs *api.CattleSet) string {
	return fmt.Sprintf("cattleset-%s-%s", cs.Namespace, cs.Name)
}

// renderSiteDeployment renders the Deployment for one Member site: the spec core
// with the site's replica count, then the site's strategic-merge sitePatch (its
// scope already validated by the webhook).
func renderSiteDeployment(cs *api.CattleSet, site SiteDesired) (*apps.Deployment, error) {
	dep := renderDeployment(cs)
	dep.Labels = renderedLabels(cs, &site.ClusterName)
	dep.Labels[api.RevisionLabel] = revisionHash(cs.Spec.Template)
	replicas := site.Replicas
	dep.Spec.Replicas = &replicas

	patch := sitePatchFor(cs, site.ClusterName)
	if len(patch) == 0 {
		return dep, nil
	}
	merged, err := applyStrategicMerge(dep, patch)
	if err != nil {
		return nil, fmt.Errorf("applying sitePatch for %s: %w", site.ClusterName, err)
	}
	// The site replica count is the controller's, never the patch's (the webhook
	// forbids a patch touching replicas, but re-assert to be safe).
	merged.Spec.Replicas = &replicas
	return merged, nil
}

func sitePatchFor(cs *api.CattleSet, cluster string) []byte {
	for _, sp := range cs.Spec.SitePatches {
		if sp.ClusterName == cluster {
			return sp.Patch.Raw
		}
	}
	return nil
}

func applyStrategicMerge(dep *apps.Deployment, patch []byte) (*apps.Deployment, error) {
	orig, err := json.Marshal(dep)
	if err != nil {
		return nil, err
	}
	merged, err := strategicpatch.StrategicMergePatch(orig, patch, apps.Deployment{})
	if err != nil {
		return nil, err
	}
	out := &apps.Deployment{}
	if err := json.Unmarshal(merged, out); err != nil {
		return nil, err
	}
	return out, nil
}

// renderPDB renders a per-site PodDisruptionBudget from the site's desired count
// and the strategy's maxUnavailable, so availability math is bounded per site.
func renderPDB(cs *api.CattleSet, site SiteDesired) *policyv1.PodDisruptionBudget {
	if site.Replicas == 0 {
		return nil
	}
	maxUnavailable := intstr.FromInt32(1)
	if ru := cs.Spec.Strategy.RollingUpdate; ru != nil && ru.MaxUnavailable != nil {
		maxUnavailable = *ru.MaxUnavailable
	}
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cs.Name,
			Namespace: cs.Namespace,
			Labels:    renderedLabels(cs, &site.ClusterName),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       cs.Spec.Selector,
		},
	}
}

// buildManifestWork packs one site's rendered objects into a ManifestWork,
// wiring the Deployment's status feedback and, for a gated Service, the
// ServerSideApply + ignoreFields on spec.selector so the OCM work agent never
// reverts the spoke gate agent's admission flip (design Section 6).
func buildManifestWork(cs *api.CattleSet, site SiteDesired, objs []client.Object, gatedServiceName string) (*apiworkv1.ManifestWork, error) {
	manifests := make([]apiworkv1.Manifest, 0, len(objs))
	var configs []apiworkv1.ManifestConfigOption

	for _, obj := range objs {
		u, err := toUnstructured(obj)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, apiworkv1.Manifest{
			RawExtension: runtime.RawExtension{Object: u},
		})

		switch typed := obj.(type) {
		case *apps.Deployment:
			configs = append(configs, deploymentFeedbackConfig(typed))
		case *core.Service:
			if gatedServiceName != "" && typed.Name == gatedServiceName {
				configs = append(configs, gatedServiceConfig(typed))
			}
		}
	}

	labels := renderedLabels(cs, &site.ClusterName)
	labels[api.ManifestWorkClusterNameLabel] = site.ClusterName

	return &apiworkv1.ManifestWork{
		TypeMeta: metav1.TypeMeta{APIVersion: "work.open-cluster-management.io/v1", Kind: "ManifestWork"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifestWorkName(cs),
			Namespace: site.ClusterName,
			Labels:    labels,
		},
		Spec: apiworkv1.ManifestWorkSpec{
			Workload:        apiworkv1.ManifestsTemplate{Manifests: manifests},
			ManifestConfigs: configs,
		},
	}, nil
}

// deploymentFeedbackConfig projects the rendered Deployment's rollout aggregates
// back to the hub via FeedbackRules.
func deploymentFeedbackConfig(dep *apps.Deployment) apiworkv1.ManifestConfigOption {
	return apiworkv1.ManifestConfigOption{
		ResourceIdentifier: apiworkv1.ResourceIdentifier{
			Group:     "apps",
			Resource:  "deployments",
			Name:      dep.Name,
			Namespace: dep.Namespace,
		},
		FeedbackRules: []apiworkv1.FeedbackRule{{
			Type: apiworkv1.JSONPathsType,
			JsonPaths: []apiworkv1.JsonPath{
				{Name: "readyReplicas", Path: ".status.readyReplicas"},
				{Name: "updatedReplicas", Path: ".status.updatedReplicas"},
				{Name: "availableReplicas", Path: ".status.availableReplicas"},
				{Name: "observedGeneration", Path: ".status.observedGeneration"},
			},
		}},
	}
}

// gatedServiceConfig makes the spoke gate agent the single writer of a gated
// Service's selector after creation: ServerSideApply with ignoreFields on
// spec.selector, so a work-agent resync never reverts an admitted flip.
func gatedServiceConfig(svc *core.Service) apiworkv1.ManifestConfigOption {
	return apiworkv1.ManifestConfigOption{
		ResourceIdentifier: apiworkv1.ResourceIdentifier{
			Group:     "",
			Resource:  "services",
			Name:      svc.Name,
			Namespace: svc.Namespace,
		},
		UpdateStrategy: &apiworkv1.UpdateStrategy{
			Type: apiworkv1.UpdateStrategyTypeServerSideApply,
			ServerSideApply: &apiworkv1.ServerSideApplyConfig{
				IgnoreFields: []apiworkv1.IgnoreField{{
					Condition: apiworkv1.IgnoreFieldsConditionOnSpokePresent,
					JSONPaths: []string{"$.spec.selector"},
				}},
			},
		},
	}
}

// deploymentFromManifestWork extracts the rendered Deployment currently carried
// by a site's ManifestWork, used to read the site's live revision and to hold
// the old pod template when a rollout is sequencing.
func deploymentFromManifestWork(mw *apiworkv1.ManifestWork) (*apps.Deployment, bool) {
	for _, m := range mw.Spec.Workload.Manifests {
		u := &unstructured.Unstructured{}
		var raw []byte
		if m.Raw != nil {
			raw = m.Raw
		} else if m.Object != nil {
			b, err := json.Marshal(m.Object)
			if err != nil {
				continue
			}
			raw = b
		} else {
			continue
		}
		if err := u.UnmarshalJSON(raw); err != nil {
			continue
		}
		if u.GetKind() != "Deployment" {
			continue
		}
		dep := &apps.Deployment{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, dep); err != nil {
			return nil, false
		}
		return dep, true
	}
	return nil, false
}

func toUnstructured(obj client.Object) (*unstructured.Unstructured, error) {
	obj.SetOwnerReferences(nil)
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}
