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

package v1

import (
	"testing"

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	kmapi "kmodules.xyz/client-go/api/v1"
)

func baseCattleSet() *api.CattleSet {
	return &api.CattleSet{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-api", Namespace: "orders"},
		Spec: api.CattleSetSpec{
			Replicas: ptr.To[int32](3),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "orders-api"}},
			Template: core.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "orders-api"}},
			},
		},
	}
}

func geoPolicy(failover bool, members ...string) *petsetapi.PlacementPolicy {
	rules := make([]petsetapi.DistributionRule, 0, len(members)+1)
	for _, m := range members {
		rules = append(rules, petsetapi.DistributionRule{ClusterName: m, Role: petsetapi.DCRoleMember})
	}
	rules = append(rules, petsetapi.DistributionRule{ClusterName: "dc-c", Role: petsetapi.DCRoleArbiter})
	csc := &petsetapi.ClusterSpreadConstraint{DistributionRules: rules}
	if failover {
		csc.FailoverPolicy = &petsetapi.FailoverPolicy{
			Trigger: petsetapi.FailoverTrigger{Scope: petsetapi.FailoverScopeGroup, Group: "orders"},
		}
	}
	return &petsetapi.PlacementPolicy{Spec: petsetapi.PlacementPolicySpec{ClusterSpreadConstraint: csc}}
}

func distributedAP() *api.CattleSet {
	cs := baseCattleSet()
	cs.Spec.Distributed = true
	cs.Spec.PodPlacementPolicy = &core.LocalObjectReference{Name: "orders-geo"}
	cs.Spec.Topology = &api.CattleTopology{Mode: api.CattleModeActivePassive}
	return cs
}

func TestValidateCattleSet(t *testing.T) {
	tests := []struct {
		name    string
		build   func() *api.CattleSet
		refs    resolvedRefs
		wantErr bool
	}{
		{
			name:  "non-distributed deployment parity is accepted",
			build: baseCattleSet,
		},
		{
			name: "PVC volume in template is rejected",
			build: func() *api.CattleSet {
				cs := baseCattleSet()
				cs.Spec.Template.Spec.Volumes = []core.Volume{{
					Name:         "data",
					VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "x"}},
				}}
				return cs
			},
			wantErr: true,
		},
		{
			name: "emptyDir volume in template is accepted",
			build: func() *api.CattleSet {
				cs := baseCattleSet()
				cs.Spec.Template.Spec.Volumes = []core.Volume{{
					Name:         "scratch",
					VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}},
				}}
				return cs
			},
		},
		{
			name: "non-distributed forbids topology",
			build: func() *api.CattleSet {
				cs := baseCattleSet()
				cs.Spec.Topology = &api.CattleTopology{Mode: api.CattleModeActivePassive}
				return cs
			},
			wantErr: true,
		},
		{
			name: "non-distributed forbids gates",
			build: func() *api.CattleSet {
				cs := baseCattleSet()
				cs.Spec.Gates = &api.CattleGates{DataTiers: []kmapi.TypedObjectReference{{Kind: "Postgres", Name: "pg"}}}
				return cs
			},
			wantErr: true,
		},
		{
			name: "non-distributed forbids traffic.export",
			build: func() *api.CattleSet {
				cs := baseCattleSet()
				cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "orders-api", Export: true}
				return cs
			},
			wantErr: true,
		},
		{
			name: "non-distributed allows traffic.serviceName and httpRoute",
			build: func() *api.CattleSet {
				cs := baseCattleSet()
				cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "orders-api", HTTPRoute: &api.CattleHTTPRoute{}}
				return cs
			},
		},
		{
			name: "distributed requires podPlacementPolicy",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.PodPlacementPolicy = nil
				return cs
			},
			refs:    resolvedRefs{pp: geoPolicy(true, "dc-a", "dc-b")},
			wantErr: true,
		},
		{
			name:    "distributed ActivePassive with failoverPolicy is accepted",
			build:   distributedAP,
			refs:    resolvedRefs{pp: geoPolicy(true, "dc-a", "dc-b")},
			wantErr: false,
		},
		{
			name:    "distributed ActivePassive without failoverPolicy is rejected",
			build:   distributedAP,
			refs:    resolvedRefs{pp: geoPolicy(false, "dc-a", "dc-b")},
			wantErr: true,
		},
		{
			name: "ActiveActive needs two Members",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.Topology.Mode = api.CattleModeActiveActive
				return cs
			},
			refs:    resolvedRefs{pp: geoPolicy(false, "dc-a")},
			wantErr: true,
		},
		{
			name: "distribution naming a non-Member is rejected",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.Topology.Mode = api.CattleModeActiveActive
				cs.Spec.Topology.Distribution = []api.CattleShare{{ClusterName: "dc-c", Weight: 1}}
				return cs
			},
			refs:    resolvedRefs{pp: geoPolicy(false, "dc-a", "dc-b")},
			wantErr: true,
		},
		{
			name: "sitePatch touching replicas is rejected",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.SitePatches = []api.SitePatch{{
					ClusterName: "dc-b",
					Patch:       runtime.RawExtension{Raw: []byte(`{"spec":{"replicas":9}}`)},
				}}
				return cs
			},
			refs:    resolvedRefs{pp: geoPolicy(true, "dc-a", "dc-b")},
			wantErr: true,
		},
		{
			name: "sitePatch touching a selector label is rejected",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.SitePatches = []api.SitePatch{{
					ClusterName: "dc-b",
					Patch:       runtime.RawExtension{Raw: []byte(`{"spec":{"template":{"metadata":{"labels":{"app":"other"}}}}}`)},
				}}
				return cs
			},
			refs:    resolvedRefs{pp: geoPolicy(true, "dc-a", "dc-b")},
			wantErr: true,
		},
		{
			name: "sitePatch touching the admission gate label is rejected",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.SitePatches = []api.SitePatch{{
					ClusterName: "dc-b",
					Patch:       runtime.RawExtension{Raw: []byte(`{"spec":{"template":{"metadata":{"labels":{"` + api.AdmittedSelectorKey + `":"true"}}}}}`)},
				}}
				return cs
			},
			refs:    resolvedRefs{pp: geoPolicy(true, "dc-a", "dc-b")},
			wantErr: true,
		},
		{
			name: "benign sitePatch (image) is accepted",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.SitePatches = []api.SitePatch{{
					ClusterName: "dc-b",
					Patch:       runtime.RawExtension{Raw: []byte(`{"spec":{"template":{"spec":{"containers":[{"name":"api","image":"harbor-b/api:1"}]}}}}`)},
				}}
				return cs
			},
			refs: resolvedRefs{pp: geoPolicy(true, "dc-a", "dc-b")},
		},
		{
			name: "gates on ActiveActive is rejected",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.Topology.Mode = api.CattleModeActiveActive
				cs.Spec.Gates = &api.CattleGates{DataTiers: []kmapi.TypedObjectReference{{Kind: "Postgres", Name: "pg"}}}
				return cs
			},
			refs:    resolvedRefs{pp: geoPolicy(false, "dc-a", "dc-b")},
			wantErr: true,
		},
		{
			name: "gates on ActivePassive with same trigger is accepted",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.Gates = &api.CattleGates{DataTiers: []kmapi.TypedObjectReference{{Kind: "Postgres", Name: "pg"}}}
				return cs
			},
			refs: resolvedRefs{
				pp:               geoPolicy(true, "dc-a", "dc-b"),
				dataTierTriggers: []*petsetapi.FailoverTrigger{{Scope: petsetapi.FailoverScopeGroup, Group: "orders"}},
			},
		},
		{
			name: "gates with a different trigger is rejected",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.Gates = &api.CattleGates{DataTiers: []kmapi.TypedObjectReference{{Kind: "Postgres", Name: "pg"}}}
				return cs
			},
			refs: resolvedRefs{
				pp:               geoPolicy(true, "dc-a", "dc-b"),
				dataTierTriggers: []*petsetapi.FailoverTrigger{{Scope: petsetapi.FailoverScopeGroup, Group: "payments"}},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := tc.build()
			errs := ValidateCattleSet(cs, tc.refs)
			if got := len(errs) > 0; got != tc.wantErr {
				t.Fatalf("ValidateCattleSet wantErr=%v got errors=%v", tc.wantErr, errs)
			}
		})
	}
}

func TestApplyDefaults(t *testing.T) {
	t.Run("upstream trio on a non-distributed object", func(t *testing.T) {
		cs := baseCattleSet()
		cs.Spec.Replicas = nil
		ApplyDefaults(cs)
		if cs.Spec.Replicas == nil || *cs.Spec.Replicas != 1 {
			t.Errorf("replicas default = %v, want 1", cs.Spec.Replicas)
		}
		if cs.Spec.RevisionHistoryLimit == nil || *cs.Spec.RevisionHistoryLimit != 10 {
			t.Errorf("revisionHistoryLimit default = %v, want 10", cs.Spec.RevisionHistoryLimit)
		}
		if cs.Spec.ProgressDeadlineSeconds == nil || *cs.Spec.ProgressDeadlineSeconds != 600 {
			t.Errorf("progressDeadlineSeconds default = %v, want 600", cs.Spec.ProgressDeadlineSeconds)
		}
		// No extension defaulting on a non-distributed object.
		if cs.Spec.RolloutOrder != "" {
			t.Errorf("rolloutOrder defaulted on non-distributed: %q", cs.Spec.RolloutOrder)
		}
	})

	t.Run("extension defaults on distributed ActivePassive", func(t *testing.T) {
		cs := distributedAP()
		ApplyDefaults(cs)
		if cs.Spec.Topology.StandbyReplicas == nil || *cs.Spec.Topology.StandbyReplicas != *cs.Spec.Replicas {
			t.Errorf("standbyReplicas default = %v, want %v", cs.Spec.Topology.StandbyReplicas, *cs.Spec.Replicas)
		}
		if cs.Spec.Topology.SurgeOnFailover == nil || cs.Spec.Topology.SurgeOnFailover.StrVal != "100%" {
			t.Errorf("surgeOnFailover default = %v, want 100%%", cs.Spec.Topology.SurgeOnFailover)
		}
		if cs.Spec.RolloutOrder != api.CattleRolloutStandbyFirst {
			t.Errorf("rolloutOrder default = %q, want StandbyFirst", cs.Spec.RolloutOrder)
		}
	})

	t.Run("rolloutOrder defaults to OneSiteAtATime for ActiveActive", func(t *testing.T) {
		cs := distributedAP()
		cs.Spec.Topology.Mode = api.CattleModeActiveActive
		ApplyDefaults(cs)
		if cs.Spec.RolloutOrder != api.CattleRolloutOneSiteAtATime {
			t.Errorf("rolloutOrder default = %q, want OneSiteAtATime", cs.Spec.RolloutOrder)
		}
	})
}

func TestValidateCattleSetUpdate(t *testing.T) {
	t.Run("selector is immutable", func(t *testing.T) {
		oldCS := baseCattleSet()
		newCS := baseCattleSet()
		newCS.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "changed"}}
		if errs := ValidateCattleSetUpdate(oldCS, newCS); len(errs) == 0 {
			t.Error("expected selector immutability error")
		}
	})

	t.Run("topology.mode is immutable", func(t *testing.T) {
		oldCS := distributedAP()
		newCS := distributedAP()
		newCS.Spec.Topology.Mode = api.CattleModeActiveActive
		if errs := ValidateCattleSetUpdate(oldCS, newCS); len(errs) == 0 {
			t.Error("expected topology.mode immutability error")
		}
	})

	t.Run("distributed true to false is rejected", func(t *testing.T) {
		oldCS := distributedAP()
		newCS := distributedAP()
		newCS.Spec.Distributed = false
		if errs := ValidateCattleSetUpdate(oldCS, newCS); len(errs) == 0 {
			t.Error("expected distributed-disable rejection")
		}
	})

	t.Run("distributed false to true is accepted", func(t *testing.T) {
		oldCS := baseCattleSet()
		newCS := distributedAP()
		if errs := ValidateCattleSetUpdate(oldCS, newCS); len(errs) != 0 {
			t.Errorf("false to true should be accepted, got %v", errs)
		}
	})
}
