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
	"strings"
	"testing"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
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
			FailoverGroupRef: &core.LocalObjectReference{Name: "orders"},
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
				cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "orders-api", Ports: httpPorts(), HTTPRoute: &api.CattleHTTPRoute{}}
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
			name: "gates was removed and is rejected even on a valid herd",
			build: func() *api.CattleSet {
				cs := distributedAP()
				cs.Spec.Gates = &api.CattleGates{DataTiers: []kmapi.TypedObjectReference{{Kind: "Postgres", Name: "pg"}}}
				return cs
			},
			refs:    resolvedRefs{pp: geoPolicy(true, "dc-a", "dc-b")},
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

	// Auto makes the mode a policy, not an identity: changing it never recreates
	// objects, and the controller only ever relaxes on a spec change.
	t.Run("topology.mode is mutable", func(t *testing.T) {
		oldCS := distributedAP()
		oldCS.Spec.Topology.Mode = api.CattleModeAuto
		newCS := distributedAP()
		if errs := ValidateCattleSetUpdate(oldCS, newCS); len(errs) != 0 {
			t.Errorf("Auto to ActivePassive must be accepted, got %v", errs)
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

func httpPorts() []core.ServicePort {
	return []core.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}}
}

func dbGroup(members ...petsetapi.FailoverGroupMember) *petsetapi.FailoverGroup {
	return &petsetapi.FailoverGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-app"},
		Spec:       petsetapi.FailoverGroupSpec{DependsOn: []string{"pg-docs"}},
	}
}

func policyWithGroup(group string, members ...string) *petsetapi.PlacementPolicy {
	pp := geoPolicy(true, members...)
	if group == "" {
		pp.Spec.ClusterSpreadConstraint.FailoverPolicy.FailoverGroupRef = nil
	} else {
		pp.Spec.ClusterSpreadConstraint.FailoverPolicy.FailoverGroupRef = &core.LocalObjectReference{Name: group}
	}
	return pp
}

func herd(mode api.CattleMode) *api.CattleSet {
	cs := distributedAP()
	cs.Spec.Topology.Mode = mode
	return cs
}

var (
	postgres  = petsetapi.FailoverGroupMember{APIGroup: "kubedb.com", Kind: "Postgres", Namespace: "demo", Name: "pg-docs"}
	cassandra = petsetapi.FailoverGroupMember{APIGroup: "kubedb.com", Kind: "Cassandra", Namespace: "demo", Name: "feed"}
)

func TestValidateRules(t *testing.T) {
	good := resolvedRefs{pp: policyWithGroup("orders-app", "dc-a", "dc-b")}
	tests := []struct {
		name    string
		build   func() *api.CattleSet
		refs    resolvedRefs
		wantErr string // substring of the error; "" means accepted
	}{
		{"a normal Auto herd is accepted", func() *api.CattleSet { return herd(api.CattleModeAuto) }, good, ""},
		{"no topology at all defaults to Auto and is accepted", func() *api.CattleSet { cs := herd(""); cs.Spec.Topology = nil; return cs }, good, ""},

		// Removed fields name what replaces them.
		{"gates names dependsOn", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Gates = &api.CattleGates{DataTiers: []kmapi.TypedObjectReference{{Kind: "Postgres", Name: "pg"}}}
			return cs
		}, good, "dependsOn"},
		{"traffic.export is removed", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "s", Ports: httpPorts(), Export: true}
			return cs
		}, good, "KubeSlice"},

		// Mode and group.
		{
			"Auto needs a failoverGroupRef", func() *api.CattleSet { return herd(api.CattleModeAuto) },
			resolvedRefs{pp: policyWithGroup("", "dc-a", "dc-b")},
			"failoverGroupRef",
		},
		{
			"ActivePassive needs a failoverGroupRef", func() *api.CattleSet { return herd(api.CattleModeActivePassive) },
			resolvedRefs{pp: policyWithGroup("", "dc-a", "dc-b")},
			"failoverGroupRef",
		},
		{
			"explicit ActiveActive needs none", func() *api.CattleSet { return herd(api.CattleModeActiveActive) },
			resolvedRefs{pp: policyWithGroup("", "dc-a", "dc-b")},
			"",
		},
		{
			"ActiveActive over Postgres is rejected", func() *api.CattleSet { return herd(api.CattleModeActiveActive) },
			resolvedRefs{pp: good.pp, group: dbGroup(), depMembers: []petsetapi.FailoverGroupMember{postgres}},
			"local Service",
		},
		{
			"ActiveActive over Cassandra is accepted", func() *api.CattleSet { return herd(api.CattleModeActiveActive) },
			resolvedRefs{pp: good.pp, group: dbGroup(), depMembers: []petsetapi.FailoverGroupMember{cassandra}},
			"",
		},
		{
			"ActiveActive with unresolved dependencies is deferred to the controller", func() *api.CattleSet { return herd(api.CattleModeActiveActive) },
			resolvedRefs{pp: good.pp, group: dbGroup()},
			"",
		},
		{
			"Auto over Postgres is accepted", func() *api.CattleSet { return herd(api.CattleModeAuto) },
			resolvedRefs{pp: good.pp, group: dbGroup(), depMembers: []petsetapi.FailoverGroupMember{postgres}},
			"",
		},

		// Traffic.
		{"traffic needs ports", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "s"}
			return cs
		}, good, "ports is required"},
		{"two unnamed ports are rejected", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "s", Ports: []core.ServicePort{{Port: 80}, {Port: 81}}}
			return cs
		}, good, "needs a name"},
		{"duplicate port names are rejected", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "s", Ports: []core.ServicePort{{Name: "a", Port: 80}, {Name: "a", Port: 81}}}
			return cs
		}, good, "Duplicate"},
		{"port out of range is rejected", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "s", Ports: []core.ServicePort{{Name: "a", Port: 70000}}}
			return cs
		}, good, "65535"},
		{"matchExpressions with traffic is rejected (they would be dropped silently)", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "s", Ports: httpPorts()}
			cs.Spec.Selector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpExists}}
			return cs
		}, good, "matchLabels only"},
		{"matchExpressions without traffic is fine", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Selector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpExists}}
			return cs
		}, good, ""},
		{"an Istio network is not implemented", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "s", Ports: httpPorts(), Network: &api.CattleNetwork{Type: "Istio"}}
			return cs
		}, good, "network.type"},
		{"network None is accepted", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "s", Ports: httpPorts(), Network: &api.CattleNetwork{Type: "None"}}
			return cs
		}, good, ""},

		// Admission.
		{"markerMaxAge above 30s is rejected", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Admission = &api.CattleAdmission{MarkerMaxAge: &metav1.Duration{Duration: 31 * time.Second}}
			return cs
		}, good, "must not exceed 30s"},
		{"markerMaxAge of 30s is accepted", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Admission = &api.CattleAdmission{MarkerMaxAge: &metav1.Duration{Duration: 30 * time.Second}}
			return cs
		}, good, ""},
		{"markerMaxAge may shrink", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Admission = &api.CattleAdmission{MarkerMaxAge: &metav1.Duration{Duration: 10 * time.Second}}
			return cs
		}, good, ""},
		{"a zero markerMaxAge is rejected", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Admission = &api.CattleAdmission{MarkerMaxAge: &metav1.Duration{}}
			return cs
		}, good, "positive"},
		{"a negative primaryLossGrace is rejected", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Admission = &api.CattleAdmission{PrimaryLossGrace: &metav1.Duration{Duration: -time.Second}}
			return cs
		}, good, "negative"},
		{"a zero primaryLossGrace disables it and is accepted", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Admission = &api.CattleAdmission{PrimaryLossGrace: &metav1.Duration{}}
			return cs
		}, good, ""},
		{"a long primaryLossGrace is rejected", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.Admission = &api.CattleAdmission{PrimaryLossGrace: &metav1.Duration{Duration: 5 * time.Minute}}
			return cs
		}, good, "1m"},

		// syncResources.
		{"syncResources of the three kinds is accepted", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.SyncResources = []core.TypedLocalObjectReference{{Kind: "ConfigMap", Name: "c"}, {Kind: "Secret", Name: "s"}, {Kind: "ServiceAccount", Name: "sa"}}
			return cs
		}, good, ""},
		{"syncResources of another kind is rejected", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.SyncResources = []core.TypedLocalObjectReference{{Kind: "Role", Name: "r"}}
			return cs
		}, good, "Unsupported"},
		{"syncResources with a group is rejected", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.SyncResources = []core.TypedLocalObjectReference{{APIGroup: ptr.To("example.com"), Kind: "ConfigMap", Name: "c"}}
			return cs
		}, good, "core"},
		{"duplicate syncResources are rejected", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.SyncResources = []core.TypedLocalObjectReference{{Kind: "Secret", Name: "s"}, {Kind: "Secret", Name: "s"}}
			return cs
		}, good, "Duplicate"},
		{"the same name in two kinds is fine", func() *api.CattleSet {
			cs := herd(api.CattleModeAuto)
			cs.Spec.SyncResources = []core.TypedLocalObjectReference{{Kind: "Secret", Name: "x"}, {Kind: "ConfigMap", Name: "x"}}
			return cs
		}, good, ""},
		{"syncResources on a non-distributed herd is rejected", func() *api.CattleSet {
			cs := baseCattleSet()
			cs.Spec.SyncResources = []core.TypedLocalObjectReference{{Kind: "Secret", Name: "s"}}
			return cs
		}, resolvedRefs{}, "only allowed when distributed"},
		{"admission on a non-distributed herd is rejected", func() *api.CattleSet {
			cs := baseCattleSet()
			cs.Spec.Admission = &api.CattleAdmission{}
			return cs
		}, resolvedRefs{}, "only allowed when distributed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateCattleSet(tc.build(), tc.refs)
			switch {
			case tc.wantErr == "" && len(errs) > 0:
				t.Fatalf("expected the object to be accepted, got %v", errs)
			case tc.wantErr != "" && len(errs) == 0:
				t.Fatalf("expected an error containing %q, got none", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(errs.ToAggregate().Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", errs.ToAggregate().Error(), tc.wantErr)
			}
		})
	}
}

func TestApplyDefaultsAuto(t *testing.T) {
	cs := distributedAP()
	cs.Spec.Topology = nil
	ApplyDefaults(cs)
	if cs.Spec.Topology == nil || cs.Spec.Topology.Mode != api.CattleModeAuto {
		t.Fatalf("a distributed herd with no topology gets Auto: %+v", cs.Spec.Topology)
	}
	if cs.Spec.RolloutOrder != api.CattleRolloutStandbyFirst {
		t.Errorf("Auto defaults to StandbyFirst, got %q", cs.Spec.RolloutOrder)
	}
	// An explicit mode is left alone.
	cs = distributedAP()
	cs.Spec.Topology.Mode = api.CattleModeActiveActive
	ApplyDefaults(cs)
	if cs.Spec.Topology.Mode != api.CattleModeActiveActive {
		t.Fatal("defaulting must not change an explicit mode")
	}
}

func TestWarnings(t *testing.T) {
	has := func(warns []string, sub string) bool {
		for _, w := range warns {
			if strings.Contains(w, sub) {
				return true
			}
		}
		return false
	}
	// A FailoverGroup that does not exist yet: allowed, with a warning.
	cs := herd(api.CattleModeAuto)
	if w := WarnCattleSet(cs, resolvedRefs{pp: policyWithGroup("orders-app", "dc-a", "dc-b")}, false); !has(w, "does not exist yet") {
		t.Errorf("a missing FailoverGroup must warn: %v", w)
	}
	if w := WarnCattleSet(cs, resolvedRefs{pp: policyWithGroup("orders-app", "dc-a", "dc-b"), group: dbGroup()}, false); has(w, "does not exist yet") {
		t.Errorf("an existing FailoverGroup must not warn: %v", w)
	}

	// Pod references that are not synced.
	cs = herd(api.CattleModeAuto)
	cs.Spec.Template.Spec.ServiceAccountName = "orders"
	cs.Spec.Template.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: "regcred"}}
	cs.Spec.Template.Spec.Volumes = []core.Volume{{Name: "cfg", VolumeSource: core.VolumeSource{ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: "orders-cfg"}}}}}
	cs.Spec.Template.Spec.Containers = []core.Container{{Name: "api", Env: []core.EnvVar{{Name: "DB_PASSWORD", ValueFrom: &core.EnvVarSource{SecretKeyRef: &core.SecretKeySelector{LocalObjectReference: core.LocalObjectReference{Name: "orders-db"}}}}}}}
	w := WarnCattleSet(cs, resolvedRefs{pp: policyWithGroup("orders-app", "dc-a", "dc-b"), group: dbGroup()}, false)
	for _, want := range []string{"ServiceAccount/orders", "Secret/regcred", "ConfigMap/orders-cfg", "Secret/orders-db"} {
		if !has(w, want) {
			t.Errorf("missing a warning for %s: %v", want, w)
		}
	}
	// Listing them silences the warning; the default ServiceAccount never warns.
	cs.Spec.SyncResources = []core.TypedLocalObjectReference{{Kind: "ServiceAccount", Name: "orders"}, {Kind: "Secret", Name: "regcred"}, {Kind: "ConfigMap", Name: "orders-cfg"}, {Kind: "Secret", Name: "orders-db"}}
	if w := WarnCattleSet(cs, resolvedRefs{pp: policyWithGroup("orders-app", "dc-a", "dc-b"), group: dbGroup()}, false); has(w, "not listed in syncResources") {
		t.Errorf("listed resources must not warn: %v", w)
	}
	plain := herd(api.CattleModeAuto)
	plain.Spec.Template.Spec.ServiceAccountName = "default"
	if w := WarnCattleSet(plain, resolvedRefs{pp: policyWithGroup("orders-app", "dc-a", "dc-b"), group: dbGroup()}, false); has(w, "ServiceAccount/default") {
		t.Errorf("the default ServiceAccount exists everywhere: %v", w)
	}

	// A non-distributed herd on an Arbiter hub.
	if w := WarnCattleSet(baseCattleSet(), resolvedRefs{}, true); !has(w, "Arbiter") {
		t.Errorf("a non-distributed herd on an Arbiter hub must warn: %v", w)
	}
	if w := WarnCattleSet(baseCattleSet(), resolvedRefs{}, false); len(w) != 0 {
		t.Errorf("no warning off the hub: %v", w)
	}
}
