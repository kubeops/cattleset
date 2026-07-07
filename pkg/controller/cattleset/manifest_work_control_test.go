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
	"testing"

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func distributedHerd() *api.CattleSet {
	return &api.CattleSet{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-api", Namespace: "orders"},
		Spec: api.CattleSetSpec{
			Distributed: true,
			Replicas:    ptr.To[int32](6),
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "orders-api"}},
			Template: core.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "orders-api"}},
				Spec:       core.PodSpec{Containers: []core.Container{{Name: "api", Image: "harbor.local/api:1"}}},
			},
			PodPlacementPolicy: &core.LocalObjectReference{Name: "orders-geo"},
			Topology:           &api.CattleTopology{Mode: api.CattleModeActivePassive, StandbyReplicas: ptr.To[int32](6)},
			Traffic:            &api.CattleTraffic{ServiceName: "orders-api"},
		},
	}
}

func TestRenderSiteDeploymentAppliesSitePatch(t *testing.T) {
	cs := distributedHerd()
	cs.Spec.SitePatches = []api.SitePatch{{
		ClusterName: "dc-b",
		Patch:       runtime.RawExtension{Raw: []byte(`{"spec":{"template":{"spec":{"containers":[{"name":"api","image":"harbor-b.local/api:1"}]}}}}`)},
	}}

	site := SiteDesired{ClusterName: "dc-b", Role: petsetapi.DCRoleMember, Posture: api.CattlePostureStandby, Replicas: 6}
	dep, err := renderSiteDeployment(cs, site)
	if err != nil {
		t.Fatalf("renderSiteDeployment: %v", err)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "harbor-b.local/api:1" {
		t.Errorf("sitePatch not applied: image = %q", got)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 6 {
		t.Errorf("site replica count not set: %v", dep.Spec.Replicas)
	}
	if dep.Labels[api.ClusterNameLabel] != "dc-b" {
		t.Errorf("cluster label missing: %v", dep.Labels)
	}
}

func TestBuildManifestWorkGatedService(t *testing.T) {
	cs := distributedHerd()
	cs.Spec.Gates = &api.CattleGates{DataTiers: nil} // gated marker for this render path
	site := SiteDesired{ClusterName: "dc-b", Role: petsetapi.DCRoleMember, Posture: api.CattlePostureStandby, Replicas: 6}

	dep, err := renderSiteDeployment(cs, site)
	if err != nil {
		t.Fatal(err)
	}
	svc := renderService(cs, true)
	objs := []client.Object{dep, svc}

	mw, err := buildManifestWork(cs, site, objs, svc.Name)
	if err != nil {
		t.Fatalf("buildManifestWork: %v", err)
	}

	if mw.Namespace != "dc-b" {
		t.Errorf("ManifestWork namespace = %q, want the site cluster dc-b", mw.Namespace)
	}
	if len(mw.Spec.Workload.Manifests) != 2 {
		t.Fatalf("want 2 manifests, got %d", len(mw.Spec.Workload.Manifests))
	}

	var sawDeploymentFeedback, sawGatedService bool
	for _, cfg := range mw.Spec.ManifestConfigs {
		if cfg.ResourceIdentifier.Resource == "deployments" {
			names := map[string]bool{}
			for _, rule := range cfg.FeedbackRules {
				for _, jp := range rule.JsonPaths {
					names[jp.Name] = true
				}
			}
			if names["readyReplicas"] && names["updatedReplicas"] && names["availableReplicas"] && names["observedGeneration"] {
				sawDeploymentFeedback = true
			}
		}
		if cfg.ResourceIdentifier.Resource == "services" {
			if cfg.UpdateStrategy != nil && cfg.UpdateStrategy.ServerSideApply != nil {
				for _, ig := range cfg.UpdateStrategy.ServerSideApply.IgnoreFields {
					for _, p := range ig.JSONPaths {
						if p == "$.spec.selector" {
							sawGatedService = true
						}
					}
				}
			}
		}
	}
	if !sawDeploymentFeedback {
		t.Error("Deployment feedback rules missing")
	}
	if !sawGatedService {
		t.Error("gated Service ignoreFields on spec.selector missing")
	}

	// The rendered gated Service starts unadmitted.
	if svc.Spec.Selector[api.AdmittedSelectorKey] != api.AdmittedSelectorValue {
		t.Error("gated Service should render unadmitted (admission term present)")
	}
}

func TestPostureInputMapsSpec(t *testing.T) {
	cs := distributedHerd()
	pp := &petsetapi.PlacementPolicy{
		Spec: petsetapi.PlacementPolicySpec{
			ClusterSpreadConstraint: &petsetapi.ClusterSpreadConstraint{
				DistributionRules: []petsetapi.DistributionRule{
					{ClusterName: "dc-a", Role: petsetapi.DCRoleMember},
					{ClusterName: "dc-b", Role: petsetapi.DCRoleMember},
				},
			},
		},
	}
	in := postureInput(cs, pp, "dc-a", nil)
	if in.Mode != api.CattleModeActivePassive || in.Replicas != 6 || in.StandbyReplicas != 6 {
		t.Errorf("postureInput mapped wrong: %+v", in)
	}
	if in.Holder != "dc-a" {
		t.Errorf("holder = %q, want dc-a", in.Holder)
	}
}
