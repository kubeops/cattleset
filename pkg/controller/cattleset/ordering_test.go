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
	"context"
	"testing"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	"kubeops.dev/cattleset/pkg/controller/gate"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func pgMember() petsetapi.FailoverGroupMember {
	return petsetapi.FailoverGroupMember{APIGroup: "kubedb.com", Kind: "Postgres", Namespace: "demo", Name: "pg-docs"}
}

func resolvedOrdering(depsOf ...petsetapi.FailoverGroupMember) Ordering {
	g := &petsetapi.FailoverGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders-app"}, Spec: petsetapi.FailoverGroupSpec{DependsOn: []string{"pg-docs"}}}
	ord := Ordering{GroupName: "orders-app", Group: g}
	for _, m := range depsOf {
		ord.Deps = append(ord.Deps, Dependency{Group: "pg-docs", Member: m, Camp: gate.CampFor(m.APIGroup, m.Kind)})
	}
	if len(depsOf) == 0 {
		g.Spec.DependsOn = nil
	}
	return ord
}

func herdWithMode(mode api.CattleMode) *api.CattleSet {
	cs := distributedHerd()
	cs.Spec.Topology = &api.CattleTopology{Mode: mode}
	return cs
}

func TestCampTable(t *testing.T) {
	for _, tc := range []struct {
		apiGroup, kind string
		want           gate.Camp
	}{
		{"kubedb.com", "Postgres", gate.CampSingleWriter},
		{"kubedb.com", "postgres", gate.CampSingleWriter},
		{"kubedb.com", "MSSQLServer", gate.CampSingleWriter},
		{"kubedb.com", "Kafka", gate.CampAsyncMirror},
		{"kubedb.com", "MongoDB", gate.CampQuorumNative},
		{"kubedb.com", "Cassandra", gate.CampLeaderlessGeoNative},
		{"kubedb.com", "YugabyteDB", gate.CampLeaderlessGeoNative},
		// Unvalidated and unknown rows are restrictive.
		{"kubedb.com", "Redis", gate.CampSingleWriter},
		{"kubedb.com", "PgBouncer", gate.CampSingleWriter},
		{"kubedb.com", "SomethingNew", gate.CampSingleWriter},
		{"example.com", "Cassandra", gate.CampSingleWriter}, // not a KubeDB kind: do not relax
		{"", "", gate.CampSingleWriter},
	} {
		if got := gate.CampFor(tc.apiGroup, tc.kind); got != tc.want {
			t.Errorf("gate.CampFor(%q, %q) = %q, want %q", tc.apiGroup, tc.kind, got, tc.want)
		}
	}
}

// Review R2: unresolved ordering renders a CLOSED gate, never an open one.
func TestDeriveGateNeverOpensOnMissingInformation(t *testing.T) {
	for name, tc := range map[string]struct {
		mode api.CattleMode
		ord  Ordering
		want string
	}{
		"auto, group missing":                {api.CattleModeAuto, Ordering{GroupName: "orders-app", Unresolved: ReasonFailoverGroupMissing}, ReasonFailoverGroupMissing},
		"auto, dependency has no members":    {api.CattleModeAuto, Ordering{GroupName: "orders-app", Group: &petsetapi.FailoverGroup{}, Unresolved: ReasonDependencyUnresolved}, ReasonDependencyUnresolved},
		"explicit AA, dependency unresolved": {api.CattleModeActiveActive, Ordering{GroupName: "orders-app", Group: &petsetapi.FailoverGroup{}, Unresolved: ReasonDependencyUnresolved}, ReasonDependencyUnresolved},
		"explicit AA, group missing":         {api.CattleModeActiveActive, Ordering{GroupName: "orders-app", Unresolved: ReasonFailoverGroupMissing}, ReasonFailoverGroupMissing},
		"app shares the data group":          {api.CattleModeAuto, Ordering{GroupName: "pg-docs", Group: &petsetapi.FailoverGroup{}, Unresolved: ReasonAppSharesDataGroup}, ReasonAppSharesDataGroup},
		"auto, no failoverGroupRef":          {api.CattleModeAuto, Ordering{}, ReasonNoFailoverGroup},
		"explicit AP, no failoverGroupRef":   {api.CattleModeActivePassive, Ordering{}, ReasonNoFailoverGroup},
	} {
		t.Run(name, func(t *testing.T) {
			d := deriveGate(herdWithMode(tc.mode), tc.ord, "", true)
			if d.Gate == nil || d.Gate.Mode != gate.GateModeClosed {
				t.Fatalf("gate must be Closed, got %+v", d.Gate)
			}
			if d.Reason != tc.want || d.OK {
				t.Fatalf("reason = %q ok=%v, want %q not ok", d.Reason, d.OK, tc.want)
			}
		})
	}
}

func TestDeriveGateOpenOnlyWhereThereIsNothingToDependOn(t *testing.T) {
	// An explicit ActiveActive herd that names no group at all.
	d := deriveGate(herdWithMode(api.CattleModeActiveActive), Ordering{}, "", true)
	if d.Gate != nil || d.Mode != api.CattleModeActiveActive || !d.OK {
		t.Fatalf("%+v", d)
	}
	// A group that exists with empty dependsOn: Auto is ActiveActive and ungated.
	d = deriveGate(herdWithMode(api.CattleModeAuto), resolvedOrdering(), "", true)
	if d.Gate != nil || d.Mode != api.CattleModeActiveActive {
		t.Fatalf("%+v", d)
	}
	// Explicit ActivePassive over the same group follows the herd's own marker.
	d = deriveGate(herdWithMode(api.CattleModeActivePassive), resolvedOrdering(), "", true)
	if d.Gate == nil || d.Gate.Mode != gate.GateModeAppMarker || d.Gate.AppLease != "primary-dc-orders-app" || d.Mode != api.CattleModeActivePassive {
		t.Fatalf("%+v", d)
	}
}

func TestDeriveGateOverADatabase(t *testing.T) {
	d := deriveGate(herdWithMode(api.CattleModeAuto), resolvedOrdering(pgMember()), "", true)
	if d.Mode != api.CattleModeActivePassive || !d.OK || d.Gate == nil || d.Gate.Mode != gate.GateModeTiers {
		t.Fatalf("%+v", d)
	}
	if len(d.Gate.Tiers) != 1 {
		t.Fatalf("tiers: %+v", d.Gate.Tiers)
	}
	tier := d.Gate.Tiers[0]
	if tier.Lease != "primary-dc-pg-docs" || tier.Namespace != "demo" || tier.Service != "pg-docs" || tier.Camp != gate.CampSingleWriter {
		t.Fatalf("tier: %+v", tier)
	}
	if tier.InstanceSelector["app.kubernetes.io/instance"] != "pg-docs" {
		t.Fatalf("instance selector: %+v", tier.InstanceSelector)
	}
	if d.Gate.PrimaryLossGraceSeconds != 15 || d.Gate.MarkerMaxAgeSeconds != 30 || !d.Gate.DrainPodLabel {
		t.Fatalf("defaults not carried into the gate: %+v", d.Gate)
	}
	// The rendered annotation survives the agent's strict parser.
	raw, err := d.Gate.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got := gate.ParseSpec(raw, true); got.Mode != gate.GateModeTiers || len(got.Tiers) != 1 {
		t.Fatalf("the agent would not accept what the hub renders: %+v", got)
	}

	// Explicit ActiveActive over a single writer is tightened, and flagged.
	d = deriveGate(herdWithMode(api.CattleModeActiveActive), resolvedOrdering(pgMember()), "", true)
	if d.Mode != api.CattleModeActivePassive || d.OK || d.Reason != ReasonModeConflict || d.Gate.Mode != gate.GateModeTiers {
		t.Fatalf("%+v", d)
	}
}

func TestDeriveGateLeaderless(t *testing.T) {
	cass := petsetapi.FailoverGroupMember{APIGroup: "kubedb.com", Kind: "Cassandra", Namespace: "demo", Name: "feed"}
	d := deriveGate(herdWithMode(api.CattleModeAuto), resolvedOrdering(cass), "", true)
	if d.Mode != api.CattleModeActiveActive || d.Gate == nil || d.Gate.Mode != gate.GateModeTiers || d.Gate.Tiers[0].Camp != gate.CampLeaderlessGeoNative {
		t.Fatalf("leaderless: active-active, still gated on a local instance: %+v", d)
	}
	// One non-leaderless dependency among them makes it ActivePassive.
	d = deriveGate(herdWithMode(api.CattleModeAuto), resolvedOrdering(cass, pgMember()), "", true)
	if d.Mode != api.CattleModeActivePassive {
		t.Fatalf("%+v", d)
	}
}

// Review R11: the derived mode tightens by itself and never loosens by itself.
func TestDeriveGateTightensOnly(t *testing.T) {
	cs := herdWithMode(api.CattleModeAuto)

	// A dependency's members disappear (or the group is edited): the herd was
	// ActivePassive, so it must not become an ungated ActiveActive herd.
	d := deriveGate(cs, resolvedOrdering(), api.CattleModeActivePassive, false)
	if d.Mode != api.CattleModeActivePassive || d.Reason != ReasonTightened {
		t.Fatalf("must stay ActivePassive: %+v", d)
	}
	if d.Gate == nil || d.Gate.Mode == gate.GateModeOpen {
		t.Fatalf("and must not open its gate: %+v", d.Gate)
	}
	// Once the CattleSet spec changes, the user has asked: relax.
	d = deriveGate(cs, resolvedOrdering(), api.CattleModeActivePassive, true)
	if d.Mode != api.CattleModeActiveActive || d.Gate != nil {
		t.Fatalf("a spec change may relax: %+v", d)
	}
	// Tightening itself is always allowed.
	d = deriveGate(cs, resolvedOrdering(pgMember()), api.CattleModeActiveActive, false)
	if d.Mode != api.CattleModeActivePassive {
		t.Fatalf("%+v", d)
	}
}

func TestResolveOrdering(t *testing.T) {
	sch := runtime.NewScheme()
	_ = petsetapi.AddToScheme(sch)
	_ = core.AddToScheme(sch)
	group := func(name string, deps []string, members ...petsetapi.FailoverGroupMember) *petsetapi.FailoverGroup {
		return &petsetapi.FailoverGroup{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: petsetapi.FailoverGroupSpec{DependsOn: deps}, Status: petsetapi.FailoverGroupStatus{Members: members, ActiveDC: "dc-a", Phase: petsetapi.FailoverGroupPhaseSteady}}
	}
	self := petsetapi.FailoverGroupMember{APIGroup: api.GroupName, Kind: "CattleSet", Namespace: "orders", Name: "orders-api"}

	cases := map[string]struct {
		objs       []client.Object
		group      string
		unresolved string
		deps       int
	}{
		"no group":                   {nil, "", "", 0},
		"group missing":              {nil, "orders-app", ReasonFailoverGroupMissing, 0},
		"dependency group missing":   {[]client.Object{group("orders-app", []string{"pg-docs"})}, "orders-app", ReasonDependencyUnresolved, 0},
		"dependency without members": {[]client.Object{group("orders-app", []string{"pg-docs"}), group("pg-docs", nil)}, "orders-app", ReasonDependencyUnresolved, 0},
		"resolved":                   {[]client.Object{group("orders-app", []string{"pg-docs"}, self), group("pg-docs", nil, pgMember())}, "orders-app", "", 1},
		"no dependsOn":               {[]client.Object{group("orders-app", nil, self)}, "orders-app", "", 0},
		"app shares the data group":  {[]client.Object{group("pg-docs", nil, pgMember(), self)}, "pg-docs", ReasonAppSharesDataGroup, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(sch).WithObjects(tc.objs...).Build()}
			ord, err := r.resolveOrdering(context.Background(), tc.group)
			if err != nil {
				t.Fatal(err)
			}
			if ord.Unresolved != tc.unresolved || len(ord.Deps) != tc.deps {
				t.Fatalf("unresolved=%q deps=%d (%s), want %q %d", ord.Unresolved, len(ord.Deps), ord.Message, tc.unresolved, tc.deps)
			}
		})
	}
}

// --- capacity: review G1 and design 6.3 ---

func fresh(holder string) LeaseObservation {
	return LeaseObservation{Found: true, Fresh: true, Holder: holder, Expires: time.Now().Add(time.Minute)}
}

var (
	unreadable = LeaseObservation{Err: context.DeadlineExceeded}
	expired    = LeaseObservation{Found: true, Holder: "dc-a"} // not Fresh
)

func prevActive(clusters ...string) map[string]SiteDesired {
	out := map[string]SiteDesired{}
	for _, c := range clusters {
		out[c] = SiteDesired{ClusterName: c, Posture: api.CattlePostureActive, Replicas: 6}
	}
	return out
}

func TestActiveSetHoldsWhenNothingIsKnown(t *testing.T) {
	for name, obs := range map[string]LeaseObservation{"unreadable": unreadable, "expired": expired, "missing": {}} {
		t.Run(name, func(t *testing.T) {
			active, hold := activeSet([]string{"dc-a", "dc-b"}, obs, nil, nil, prevActive("dc-a"), nil)
			if !hold {
				t.Fatal("must hold: the holder could not be established")
			}
			if !active["dc-a"] {
				t.Fatal("the serving site must stay active")
			}
		})
	}
}

// The G1 scenario end to end through ComputePosture: a warm standby (3 of 6) and
// a read error on the Lease must not scale the serving DC down.
func TestG1AReadErrorDoesNotScaleTheServingSiteDown(t *testing.T) {
	prev := prevActive("dc-a")
	prev["dc-b"] = SiteDesired{ClusterName: "dc-b", Posture: api.CattlePostureStandby, Replicas: 3}
	in := PostureInput{
		Mode: api.CattleModeActivePassive, Replicas: 6, StandbyReplicas: 3,
		Rules:    []petsetapi.DistributionRule{{ClusterName: "dc-a", Role: petsetapi.DCRoleMember}, {ClusterName: "dc-b", Role: petsetapi.DCRoleMember}},
		Previous: prev,
	}
	in.Active, in.Hold = activeSet([]string{"dc-a", "dc-b"}, unreadable, nil, nil, prev, nil)
	got := ComputePosture(in)
	if got["dc-a"].Replicas != 6 || got["dc-a"].Posture != api.CattlePostureActive {
		t.Fatalf("serving site was scaled down on a read error: %+v", got["dc-a"])
	}
	if got["dc-b"].Replicas != 3 {
		t.Fatalf("standby changed: %+v", got["dc-b"])
	}

	// The same Lease read as a definite holder behaves normally.
	in.Hold = false
	in.Active, in.Hold = activeSet([]string{"dc-a", "dc-b"}, fresh("dc-b"), nil, nil, prev, nil)
	got = ComputePosture(in)
	if got["dc-b"].Replicas != 6 {
		t.Fatalf("the new holder must scale up: %+v", got["dc-b"])
	}
}

func TestActiveSetScalesUpEarlyOnADependencyLease(t *testing.T) {
	// The database group's Lease moved to dc-b; the app's own has not yet.
	active, hold := activeSet([]string{"dc-a", "dc-b"}, fresh("dc-a"), []LeaseObservation{fresh("dc-b")}, nil, prevActive("dc-a"), nil)
	if hold || !active["dc-b"] || !active["dc-a"] {
		t.Fatalf("dc-b must scale up early and dc-a must not scale down yet: active=%v hold=%v", active, hold)
	}
}

func TestActiveSetScalesUpOnAdmission(t *testing.T) {
	active, _ := activeSet([]string{"dc-a", "dc-b"}, fresh("dc-a"), nil, map[string]bool{"dc-b": true}, nil, nil)
	if !active["dc-b"] {
		t.Fatal("an admitted site is serving and needs full capacity")
	}
}

func TestActiveSetScalesDownOnlyWhenSteadyElsewhere(t *testing.T) {
	members := []string{"dc-a", "dc-b"}
	prev := prevActive("dc-a")
	group := func(phase petsetapi.FailoverGroupPhase, active string) *petsetapi.FailoverGroup {
		return &petsetapi.FailoverGroup{Status: petsetapi.FailoverGroupStatus{Phase: phase, ActiveDC: active}}
	}
	// The Lease holder is dc-b already, but the group is still moving.
	active, _ := activeSet(members, fresh("dc-b"), nil, nil, prev, group(petsetapi.FailoverGroupPhaseWaitingForMembers, "dc-a"))
	if !active["dc-a"] {
		t.Fatal("dc-a must keep full capacity until the group is Steady on dc-b")
	}
	// Steady on dc-b: now dc-a may drop.
	active, _ = activeSet(members, fresh("dc-b"), nil, nil, prev, group(petsetapi.FailoverGroupPhaseSteady, "dc-b"))
	if active["dc-a"] {
		t.Fatal("dc-a may scale down once the group is Steady on dc-b")
	}
	// Steady, but on dc-a itself: nothing to scale down.
	active, _ = activeSet(members, fresh("dc-a"), nil, nil, prev, group(petsetapi.FailoverGroupPhaseSteady, "dc-a"))
	if !active["dc-a"] || active["dc-b"] {
		t.Fatalf("%v", active)
	}
}

func TestComputePostureHoldWithoutHistoryFallsBackToStandby(t *testing.T) {
	in := PostureInput{
		Mode: api.CattleModeActivePassive, Replicas: 6, StandbyReplicas: 2, Hold: true,
		Rules: []petsetapi.DistributionRule{{ClusterName: "dc-a", Role: petsetapi.DCRoleMember}},
	}
	if got := ComputePosture(in); got["dc-a"].Replicas != 2 || got["dc-a"].Posture != api.CattlePostureStandby {
		t.Fatalf("a first reconcile with no information starts at the standby count: %+v", got["dc-a"])
	}
}
