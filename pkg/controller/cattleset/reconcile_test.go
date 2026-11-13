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
	"strings"
	"testing"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	"kubeops.dev/cattleset/pkg/controller/gate"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ctx = context.Background()

// steady is a hub where the database and the application are both active on
// spoke-1 and everything is healthy.
func steady(t *testing.T, extra ...client.Object) *harness {
	h := newHarness(t, "spoke-1", "spoke-1", extra...)
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.leaseHeld("primary-dc-pg-docs", "spoke-1")
	h.dcHealthy("spoke-1", "spoke-2")
	h.reconcile() // creates the ManifestWorks
	return h
}

// --- review R2: the gate never opens on missing information ---

func TestReconcileClosedGateWhenTheFailoverGroupIsMissing(t *testing.T) {
	h := newHarness(t, "spoke-1", "spoke-1")
	if err := h.c.Delete(ctx, &petsetapi.FailoverGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders-app"}}); err != nil {
		t.Fatal(err)
	}
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.dcHealthy("spoke-1", "spoke-2")
	h.reconcile()

	for _, site := range []string{"spoke-1", "spoke-2"} {
		spec, svc := h.gateOf(site)
		if spec.Mode != gate.GateModeClosed {
			t.Errorf("%s: gate mode = %q, want Closed", site, spec.Mode)
		}
		if svc.Spec.Selector[api.AdmittedSelectorKey] == "" {
			t.Errorf("%s: the Service must start unadmitted", site)
		}
	}
	if c := h.cond(api.ConditionOrderingConfigured); c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonFailoverGroupMissing {
		t.Errorf("OrderingConfigured = %+v", c)
	}
	if h.cs().Status.DerivedMode != api.CattleModeActivePassive {
		t.Errorf("derivedMode = %q", h.cs().Status.DerivedMode)
	}
}

func TestReconcileClosedGateWhileADependencyHasNoMembers(t *testing.T) {
	h := newHarness(t, "spoke-1", "spoke-1")
	pg := h.group("pg-docs")
	pg.Status.Members = nil
	if err := h.c.Status().Update(ctx, pg); err != nil {
		t.Fatal(err)
	}
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.dcHealthy("spoke-1", "spoke-2")
	h.reconcile()
	if spec, _ := h.gateOf("spoke-1"); spec.Mode != gate.GateModeClosed {
		t.Fatalf("before the database operator reports, the gate must be Closed, got %q", spec.Mode)
	}
}

func TestReconcileRendersTheTiersGateOverPostgres(t *testing.T) {
	h := steady(t)
	spec, svc := h.gateOf("spoke-1")
	if spec.Mode != gate.GateModeTiers || len(spec.Tiers) != 1 {
		t.Fatalf("gate = %+v", spec)
	}
	tier := spec.Tiers[0]
	if tier.Lease != "primary-dc-pg-docs" || tier.Namespace != "demo" || tier.Service != "pg-docs" || tier.Camp != gate.CampSingleWriter {
		t.Fatalf("tier = %+v", tier)
	}
	if _, ok := svc.Spec.Selector[api.AdmittedSelectorKey]; !ok {
		t.Fatal("a gated Service starts unadmitted")
	}
	if svc.Spec.Selector["app"] != "orders-api" {
		t.Fatalf("selector = %v", svc.Spec.Selector)
	}
	// Review G2: the ports are the user's, not a hard-coded 80 to 8080.
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].Name != "http" {
		t.Fatalf("ports = %+v", svc.Spec.Ports)
	}
	if cs := h.cs(); cs.Status.DerivedMode != api.CattleModeActivePassive || len(cs.Status.Dependencies) != 1 || cs.Status.Dependencies[0].Camp != string(gate.CampSingleWriter) {
		t.Fatalf("status: mode=%q deps=%+v", cs.Status.DerivedMode, cs.Status.Dependencies)
	}
	if c := h.cond(api.ConditionOrderingConfigured); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("OrderingConfigured = %+v", c)
	}
}

func TestReconcileRendersNoGateWithNothingToDependOn(t *testing.T) {
	h := newHarness(t, "spoke-1", "spoke-1")
	g := h.group("orders-app")
	g.Spec.DependsOn = nil
	if err := h.c.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.dcHealthy("spoke-1", "spoke-2")
	h.reconcile()
	spec, svc := h.gateOf("spoke-1")
	if spec.Mode != gate.GateModeClosed {
		// ParseSpec reads an absent annotation as Closed, so a Service with no gate
		// at all and no admission term is how Open is rendered.
		if _, gated := svc.Spec.Selector[api.AdmittedSelectorKey]; gated {
			t.Fatalf("an ungated herd's Service must carry no admission term: %v", svc.Spec.Selector)
		}
	}
	if h.cs().Status.DerivedMode != api.CattleModeActiveActive {
		t.Fatalf("derivedMode = %q", h.cs().Status.DerivedMode)
	}
}

// --- review G1: missing information never scales a serving site down ---

func TestReconcileHoldsCapacityWhenTheLeaseCannotBeRead(t *testing.T) {
	h := steady(t)
	if got := h.desired("spoke-1"); got != 6 {
		t.Fatalf("setup: spoke-1 = %d, want 6", got)
	}
	if got := h.desired("spoke-2"); got != 3 {
		t.Fatalf("setup: spoke-2 = %d, want the standby count 3", got)
	}

	// The coordination plane stops answering.
	h.coord.leases = map[string]LeaseObservation{
		"primary-dc-orders-app": {Err: context.DeadlineExceeded},
		"primary-dc-pg-docs":    {Err: context.DeadlineExceeded},
		"dc-health-spoke-1":     {Err: context.DeadlineExceeded},
		"dc-health-spoke-2":     {Err: context.DeadlineExceeded},
	}
	h.reconcile()
	if got := h.desired("spoke-1"); got != 6 {
		t.Fatalf("the serving site was scaled down to %d on a read error", got)
	}
	if got := h.desired("spoke-2"); got != 3 {
		t.Fatalf("the standby changed to %d", got)
	}
	if c := h.cond(api.ConditionCapacityHeld); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("CapacityHeld = %+v", c)
	}
	if c := h.cond(api.ConditionLeaseAccessible); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("LeaseAccessible = %+v", c)
	}

	// An expired Lease is just as unknown.
	h.coord.leases["primary-dc-orders-app"] = LeaseObservation{Found: true, Holder: "spoke-1"}
	h.coord.leases["primary-dc-pg-docs"] = LeaseObservation{Found: true, Holder: "spoke-1"}
	h.reconcile()
	if got := h.desired("spoke-1"); got != 6 {
		t.Fatalf("an expired Lease scaled the serving site down to %d", got)
	}

	// The Lease comes back: capacity is no longer held.
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.leaseHeld("primary-dc-pg-docs", "spoke-1")
	h.dcHealthy("spoke-1", "spoke-2")
	h.reconcile()
	if c := h.cond(api.ConditionCapacityHeld); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("CapacityHeld = %+v", c)
	}
}

func TestReconcileScalesTheNewSiteUpOnTheDependencyLease(t *testing.T) {
	h := steady(t)
	// The database group's Lease moves to spoke-2 (about 40 s before the database
	// is writable); the application's own Lease has not moved yet.
	h.leaseHeld("primary-dc-pg-docs", "spoke-2")
	h.reconcile()
	if got := h.desired("spoke-2"); got != 6 {
		t.Fatalf("spoke-2 = %d, want full capacity early", got)
	}
	if got := h.desired("spoke-1"); got != 6 {
		t.Fatalf("spoke-1 = %d, must keep full capacity until the group is Steady elsewhere", got)
	}
}

// --- review G5 and G3 ---

func TestReconcileAddsAFinalizerAndCleansUpOnDelete(t *testing.T) {
	h := steady(t)
	if !containsString(h.cs().Finalizers, api.CleanupFinalizer) {
		t.Fatalf("finalizers = %v", h.cs().Finalizers)
	}
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.reconcile() // reports the member
	if h.member() == nil {
		t.Fatal("setup: the herd should be reported to its FailoverGroup")
	}

	if err := h.c.Delete(ctx, h.cs()); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	for _, site := range []string{"spoke-1", "spoke-2"} {
		if h.hasMW(site, manifestWorkName(herdCattleSet())) || h.hasMW(site, depsWorkName(herdCattleSet())) {
			t.Errorf("%s: the ManifestWorks must be removed with the CattleSet", site)
		}
	}
	if h.member() != nil {
		t.Error("the herd must be withdrawn from its FailoverGroup, or the group waits on it forever")
	}
	if err := h.c.Get(ctx, types.NamespacedName{Namespace: "orders", Name: "orders-api"}, &api.CattleSet{}); err == nil {
		t.Error("the CattleSet should be gone once the finalizer is removed")
	}
}

func TestReconcilePrunesAClusterThatIsNoLongerAMember(t *testing.T) {
	h := steady(t)
	if !h.hasMW("spoke-2", manifestWorkName(h.cs())) {
		t.Fatal("setup")
	}
	pp := &petsetapi.PlacementPolicy{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "orders-geo"}, pp); err != nil {
		t.Fatal(err)
	}
	rules := pp.Spec.ClusterSpreadConstraint.DistributionRules
	pp.Spec.ClusterSpreadConstraint.DistributionRules = []petsetapi.DistributionRule{rules[0], rules[2]}
	if err := h.c.Update(ctx, pp); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if h.hasMW("spoke-2", manifestWorkName(h.cs())) || h.hasMW("spoke-2", depsWorkName(h.cs())) {
		t.Error("spoke-2 left the placement policy; its workload must be removed")
	}
	if !h.hasMW("spoke-1", manifestWorkName(h.cs())) {
		t.Error("spoke-1 must be untouched")
	}
}

func syncedObjects(t *testing.T, h *harness, cluster string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, u := range manifestsOf(t, h.mw(cluster, depsWorkName(h.cs()))) {
		out[u.GetKind()+"/"+u.GetName()] = true
	}
	return out
}

func TestReconcileDeliversPodDependencies(t *testing.T) {
	cm := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "orders", Name: "orders-cfg", UID: "x", ResourceVersion: "9"}, Data: map[string]string{"k": "v"}}
	sec := &core.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "orders", Name: "orders-db"}, Data: map[string][]byte{"password": []byte("s3cret")}, Type: core.SecretTypeOpaque}
	sa := &core.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "orders", Name: "orders"}, Secrets: []core.ObjectReference{{Name: "token"}}}
	h := newHarness(t, "spoke-1", "spoke-1", cm, sec, sa)
	cs := h.cs()
	cs.Spec.SyncResources = []core.TypedLocalObjectReference{{Kind: "ConfigMap", Name: "orders-cfg"}, {Kind: "Secret", Name: "orders-db"}, {Kind: "ServiceAccount", Name: "orders"}}
	if err := h.c.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.dcHealthy("spoke-1", "spoke-2")
	h.reconcile()

	for _, site := range []string{"spoke-1", "spoke-2"} {
		got := syncedObjects(t, h, site)
		for _, want := range []string{"Namespace/orders", "ConfigMap/orders-cfg", "Secret/orders-db", "ServiceAccount/orders"} {
			if !got[want] {
				t.Errorf("%s: %s is not delivered: %v", site, want, got)
			}
		}
	}
	// The hub's identity is stripped from the copy, and a ServiceAccount's token
	// Secrets are not copied.
	for _, u := range manifestsOf(t, h.mw("spoke-1", depsWorkName(h.cs()))) {
		if u.GetKind() == "ConfigMap" && (u.GetUID() != "" || u.GetResourceVersion() != "") {
			t.Errorf("the copy carries the hub object's identity: %v", u.Object["metadata"])
		}
		if u.GetKind() == "ServiceAccount" {
			if _, has := u.Object["secrets"]; has {
				t.Error("a ServiceAccount's token Secrets must not be copied")
			}
		}
	}
	if c := h.cond(api.ConditionDependenciesSynced); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NotApplied" {
		t.Errorf("not yet applied on the spokes: %+v", c)
	}
	h.siteUp("spoke-1", siteState{available: 6, applied: true})
	h.siteUp("spoke-2", siteState{available: 3, applied: true})
	h.reconcile()
	if c := h.cond(api.ConditionDependenciesSynced); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("applied everywhere: %+v", c)
	}

	// An entry removed from the list is removed from the spokes.
	cs = h.cs()
	cs.Spec.SyncResources = cs.Spec.SyncResources[:1]
	if err := h.c.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	got := syncedObjects(t, h, "spoke-1")
	if got["Secret/orders-db"] || got["ServiceAccount/orders"] || !got["ConfigMap/orders-cfg"] {
		t.Errorf("after trimming the list: %v", got)
	}
}

// A source that disappears from the hub must not take the copies off the spokes
// with it: nothing is removed because of a read.
func TestReconcileKeepsCopiesWhenASourceIsMissing(t *testing.T) {
	sec := &core.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "orders", Name: "orders-db"}, Data: map[string][]byte{"password": []byte("s3cret")}}
	h := newHarness(t, "spoke-1", "spoke-1", sec)
	cs := h.cs()
	cs.Spec.SyncResources = []core.TypedLocalObjectReference{{Kind: "Secret", Name: "orders-db"}}
	if err := h.c.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.dcHealthy("spoke-1", "spoke-2")
	h.reconcile()
	if !syncedObjects(t, h, "spoke-1")["Secret/orders-db"] {
		t.Fatal("setup")
	}

	if err := h.c.Delete(ctx, sec); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if !syncedObjects(t, h, "spoke-1")["Secret/orders-db"] {
		t.Error("the copy on the spoke must be kept when the source is missing")
	}
	if c := h.cond(api.ConditionDependenciesSynced); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "SourceMissing" || !strings.Contains(c.Message, "Secret/orders-db") {
		t.Errorf("DependenciesSynced = %+v", c)
	}
	// The workload is still rendered: one missing Secret does not stop the herd.
	if !h.hasMW("spoke-1", manifestWorkName(h.cs())) {
		t.Error("the Deployment ManifestWork must still be applied")
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- review R3 and R1: what the herd reports to its FailoverGroup ---

func admittedUp(h *harness, now time.Time) {
	h.siteUp("spoke-1", siteState{available: 6, admission: gate.ReasonAdmitted, beat: now})
	h.siteUp("spoke-2", siteState{available: 3, admission: gate.ReasonNotActiveHere, beat: now})
}

func TestReconcileReportsReadyWhenAdmittedAndHealthy(t *testing.T) {
	h := steady(t)
	admittedUp(h, h.now)
	h.reconcile()
	m := h.member()
	if m == nil || m.DC != "spoke-1" || !m.Ready || m.ObservedAt == nil {
		t.Fatalf("member = %+v", m)
	}
	cs := h.cs()
	if len(cs.Status.ServingDCs) != 1 || cs.Status.ServingDCs[0] != "spoke-1" {
		t.Fatalf("servingDCs = %v", cs.Status.ServingDCs)
	}
	if c := h.cond(api.ConditionAdmitted); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Admitted = %+v", c)
	}
	for _, s := range cs.Status.Sites {
		switch s.ClusterName {
		case "spoke-1":
			if !s.Admitted || !s.Healthy || s.AdmissionReason != gate.ReasonAdmitted {
				t.Errorf("spoke-1 site status = %+v", s)
			}
		case "spoke-2":
			if s.Admitted || s.AdmissionReason != gate.ReasonNotActiveHere {
				t.Errorf("spoke-2 site status = %+v", s)
			}
		}
	}
}

// Review R3: a spoke that died keeps its last ManifestWork feedback forever. It
// must not keep the herd "ready" with it.
func TestReconcileADeadDCIsNeverReady(t *testing.T) {
	h := steady(t)
	admittedUp(h, h.now)
	h.reconcile()
	if m := h.member(); m == nil || !m.Ready {
		t.Fatalf("setup: member = %+v", m)
	}

	h.dcDead("spoke-1") // its health Lease expired; its ManifestWork still says 6 ready
	h.now = h.now.Add(2 * time.Minute)
	h.reconcile()
	m := h.member()
	if m == nil || m.Ready {
		t.Fatalf("a dead DC must not be reported ready: %+v", m)
	}
	for _, s := range h.cs().Status.Sites {
		if s.ClusterName == "spoke-1" && (s.Healthy || s.Ready != 0 || s.Admitted) {
			t.Errorf("a dead site must report nothing ready or admitted: %+v", s)
		}
	}
}

// Review R1, end to end: after a planned database switchover A to B, the
// application on A is unadmitted (its database is gone) and must still report
// ready, or the FailoverGroup controller never asks for the switchover.
func TestReconcileTheOldHolderStaysReadyAfterTheDatabaseMoves(t *testing.T) {
	h := steady(t)
	admittedUp(h, h.now)
	h.reconcile()

	// Database quiescing: unadmitted, but the data tier is still active here.
	h.siteUp("spoke-1", siteState{available: 6, admission: gate.ReasonQuiescing, beat: h.now})
	h.reconcile()
	if m := h.member(); m == nil || m.Ready {
		t.Fatalf("while the data tier is still here, an unadmitted site is not ready: %+v", m)
	}

	// The database is now active on spoke-2. The application on spoke-1 is
	// unadmitted because of that, and is ready.
	h.setDepActive("spoke-2")
	h.siteUp("spoke-1", siteState{available: 6, admission: gate.ReasonNotActiveHere, beat: h.now})
	h.reconcile()
	m := h.member()
	if m == nil || m.DC != "spoke-1" || !m.Ready {
		t.Fatalf("the old holder must report ready once the dependency is elsewhere: %+v", m)
	}
	if c := h.cond(api.ConditionDependenciesReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("DependenciesReady = %+v", c)
	}

	// But a dead DC is still not ready, whatever the dependency says.
	h.dcDead("spoke-1")
	h.reconcile()
	if m := h.member(); m.Ready {
		t.Fatal("R3 again: a dead old holder must not be ready")
	}
}

func TestReconcileReportsNothingWhenTheLeaseHolderIsUnknown(t *testing.T) {
	h := steady(t)
	admittedUp(h, h.now)
	h.reconcile()
	before := h.member()
	h.coord.leases["primary-dc-orders-app"] = LeaseObservation{Err: context.DeadlineExceeded}
	h.now = h.now.Add(2 * time.Minute)
	h.reconcile()
	after := h.member()
	if after == nil || !after.ObservedAt.Equal(before.ObservedAt) {
		t.Fatalf("with no established holder the report must be left alone (it expires on its own), before=%+v after=%+v", before, after)
	}
}

func TestReconcileFlagsAStaleGateAgent(t *testing.T) {
	h := steady(t)
	h.siteUp("spoke-1", siteState{available: 6, admission: gate.ReasonAdmitted, beat: h.now.Add(-5 * time.Minute)})
	h.siteUp("spoke-2", siteState{available: 3, admission: gate.ReasonNotActiveHere, beat: h.now})
	h.reconcile()
	if c := h.cond(api.ConditionGateAgentStale); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("GateAgentStale = %+v", c)
	}
	for _, s := range h.cs().Status.Sites {
		if s.ClusterName == "spoke-1" && s.Admitted {
			t.Error("an admission nobody has confirmed lately is not reported as admitted")
		}
	}
	h.siteUp("spoke-1", siteState{available: 6, admission: gate.ReasonAdmitted, beat: h.now})
	h.reconcile()
	if c := h.cond(api.ConditionGateAgentStale); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("GateAgentStale = %+v", c)
	}
}

// --- the planned switchover (design 4.4) ---

func TestReconcileSwitchoverRequestsTheHandoff(t *testing.T) {
	h := steady(t)
	admittedUp(h, h.now)
	// The database has moved to spoke-2 and the FailoverGroup controller asked.
	h.setDepActive("spoke-2")
	cs := h.cs()
	cs.Annotations = map[string]string{api.AnnSwitchoverTo: "spoke-2"}
	if err := h.c.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	h.siteUp("spoke-1", siteState{available: 6, admission: gate.ReasonNotActiveHere, beat: h.now})
	h.siteUp("spoke-2", siteState{available: 3, admission: gate.ReasonAdmitted, beat: h.now})
	h.reconcile()

	m := h.member()
	if m == nil || m.HandoffTo != "spoke-2" {
		t.Fatalf("the target has capacity, so the herd must ask for the handoff: %+v", m)
	}
	if c := h.cond(api.ConditionSwitchoverInProgress); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("SwitchoverInProgress = %+v", c)
	}
	if _, still := h.cs().Annotations[api.AnnSwitchoverTo]; !still {
		t.Fatal("the annotation stays until the Lease holder is the target")
	}

	// dr-controlplane moved the Lease: the switchover is done, and every trace of
	// it is removed, or the group can never be asked again.
	h.leaseHeld("primary-dc-orders-app", "spoke-2")
	h.reconcile()
	if _, still := h.cs().Annotations[api.AnnSwitchoverTo]; still {
		t.Error("the switchover-to annotation must be removed when the Lease holder is the target")
	}
	h.reconcile()
	if m := h.member(); m == nil || m.DC != "spoke-2" || m.HandoffTo != "" {
		t.Fatalf("the member must now be on spoke-2 with the request withdrawn: %+v", m)
	}
}

func TestReconcileSwitchoverWaitsForCapacityOnTheTarget(t *testing.T) {
	h := steady(t)
	h.setDepActive("spoke-2")
	cs := h.cs()
	cs.Annotations = map[string]string{api.AnnSwitchoverTo: "spoke-2"}
	if err := h.c.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	h.siteUp("spoke-1", siteState{available: 6, admission: gate.ReasonNotActiveHere, beat: h.now})
	h.siteUp("spoke-2", siteState{available: 0, admission: gate.ReasonNotActiveHere, beat: h.now}) // no pods yet
	h.reconcile()
	if m := h.member(); m == nil || m.HandoffTo != "" {
		t.Fatalf("the target has no pods yet: no handoff request, got %+v", m)
	}
	if c := h.cond(api.ConditionSwitchoverInProgress); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("SwitchoverInProgress = %+v", c)
	}
}

func TestReconcileSwitchoverIsRejectedWhenItCouldNeverConverge(t *testing.T) {
	h := steady(t) // the database is on spoke-1
	cs := h.cs()
	cs.Annotations = map[string]string{api.AnnSwitchoverTo: "spoke-2"}
	if err := h.c.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	h.siteUp("spoke-1", siteState{available: 6, admission: gate.ReasonAdmitted, beat: h.now})
	h.siteUp("spoke-2", siteState{available: 3, admission: gate.ReasonNotActiveHere, beat: h.now})
	h.reconcile()
	if _, still := h.cs().Annotations[api.AnnSwitchoverTo]; still {
		t.Fatal("a request the data tier does not support must be dropped")
	}
	if m := h.member(); m == nil || m.HandoffTo != "" {
		t.Fatalf("no handoff for a rejected request: %+v", m)
	}
	if ev := drain(h.rec); !strings.Contains(ev, "SwitchoverRejected") {
		t.Errorf("events: %q", ev)
	}
}

func TestReconcileSwitchoverAbort(t *testing.T) {
	h := steady(t)
	h.setDepActive("spoke-2")
	cs := h.cs()
	cs.Annotations = map[string]string{api.AnnSwitchoverTo: "spoke-2"}
	_ = h.c.Update(ctx, cs)
	h.siteUp("spoke-1", siteState{available: 6, admission: gate.ReasonNotActiveHere, beat: h.now})
	h.siteUp("spoke-2", siteState{available: 3, admission: gate.ReasonAdmitted, beat: h.now})
	h.reconcile()
	if m := h.member(); m == nil || m.HandoffTo != "spoke-2" {
		t.Fatalf("setup: %+v", m)
	}

	cs = h.cs()
	cs.Annotations[api.AnnSwitchoverAbort] = "true"
	_ = h.c.Update(ctx, cs)
	h.reconcile()
	if len(h.cs().Annotations) != 0 {
		t.Fatalf("an abort removes both annotations: %v", h.cs().Annotations)
	}
	h.reconcile()
	if m := h.member(); m == nil || m.HandoffTo != "" {
		t.Fatalf("an abort withdraws the request: %+v", m)
	}
}

func TestReconcileSwitchoverTimesOut(t *testing.T) {
	h := steady(t)
	h.setDepActive("spoke-2")
	cs := h.cs()
	cs.Annotations = map[string]string{api.AnnSwitchoverTo: "spoke-2"}
	_ = h.c.Update(ctx, cs)
	h.siteUp("spoke-1", siteState{available: 6, admission: gate.ReasonNotActiveHere, beat: h.now})
	h.siteUp("spoke-2", siteState{available: 0, beat: h.now}) // never gets capacity
	h.reconcile()
	if _, ok := h.cs().Annotations[api.AnnSwitchoverTo]; !ok {
		t.Fatal("setup: still running")
	}
	h.now = h.now.Add(SwitchoverTimeout + time.Minute)
	h.dcHealthy("spoke-1", "spoke-2")
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.leaseHeld("primary-dc-pg-docs", "spoke-2")
	h.reconcile()
	if _, ok := h.cs().Annotations[api.AnnSwitchoverTo]; ok {
		t.Fatal("a switchover that ran past the bound must be abandoned and its annotation removed")
	}
}

func drain(r *record.FakeRecorder) string {
	var out []string
	for {
		select {
		case e := <-r.Events:
			out = append(out, e)
		default:
			return strings.Join(out, "\n")
		}
	}
}

// --- review G8: the Arbiter hub ---

func TestReconcileRetiresTheHubLocalWorkloadOnAnArbiterHub(t *testing.T) {
	h := newHarness(t, "spoke-1", "spoke-1")
	h.r.LocalClusterName = "hub"
	cs := h.cs()
	local := renderDeployment(cs)
	local.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(cs, api.SchemeGroupVersion.WithKind(api.ResourceKindCattleSet))}
	stranger := renderDeployment(cs)
	stranger.Name = "someone-elses"
	if err := h.c.Create(ctx, local); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Create(ctx, stranger); err != nil {
		t.Fatal(err)
	}
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.leaseHeld("primary-dc-pg-docs", "spoke-1")
	h.dcHealthy("spoke-1", "spoke-2")
	h.reconcile()

	// No Member is serving yet: nothing is removed.
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(local), local.DeepCopy()); err != nil {
		t.Fatal("the hub-local Deployment must stay until a Member serves")
	}
	admittedUp(h, h.now)
	h.reconcile()
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(local), local.DeepCopy()); err == nil {
		t.Error("once a Member serves, the Deployment left on the Arbiter hub must be removed")
	}
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(stranger), stranger.DeepCopy()); err != nil {
		t.Error("an object this CattleSet does not control must never be deleted")
	}
}

func TestHubLocalIsLeftAloneWhenTheHubIsAMember(t *testing.T) {
	h := newHarness(t, "spoke-1", "spoke-1")
	h.r.LocalClusterName = "spoke-1" // the controller runs in a Member
	cs := h.cs()
	local := renderDeployment(cs)
	local.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(cs, api.SchemeGroupVersion.WithKind(api.ResourceKindCattleSet))}
	if err := h.c.Create(ctx, local); err != nil {
		t.Fatal(err)
	}
	h.leaseHeld("primary-dc-orders-app", "spoke-1")
	h.leaseHeld("primary-dc-pg-docs", "spoke-1")
	h.dcHealthy("spoke-1", "spoke-2")
	h.reconcile()
	admittedUp(h, h.now)
	h.reconcile()
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(local), local.DeepCopy()); err != nil {
		t.Fatal("a Member's local Deployment is adopted, not retired")
	}
}

// --- review G6: Lease events ---

func TestLeasePredicateIgnoresRenewals(t *testing.T) {
	pred := leasePredicate()
	mk := func(name, holder string, ann map[string]string, renewed time.Time) *coordLease {
		return newCoordLease(name, holder, ann, renewed)
	}
	old := mk("primary-dc-orders-app", "spoke-1", nil, time.Unix(100, 0))

	if pred.Update(updateEvent(old, mk("primary-dc-orders-app", "spoke-1", nil, time.Unix(105, 0)))) {
		t.Error("a plain renewal must not reconcile anything")
	}
	if !pred.Update(updateEvent(old, mk("primary-dc-orders-app", "spoke-2", nil, time.Unix(105, 0)))) {
		t.Error("a holder change must")
	}
	if !pred.Update(updateEvent(old, mk("primary-dc-orders-app", "spoke-1", map[string]string{"dr.open-cluster-management.io/follow-dc": "spoke-2"}, time.Unix(105, 0)))) {
		t.Error("an annotation change (follow-dc, handoff-to) must")
	}
	health := mk("dc-health-spoke-1", "spoke-1", nil, time.Unix(100, 0))
	if pred.Update(updateEvent(health, mk("dc-health-spoke-1", "spoke-1", nil, time.Unix(105, 0)))) {
		t.Error("a health Lease renewal is never an event: its expiry is caught by the timer")
	}
	if !pred.Create(createEvent(health)) || !pred.Delete(deleteEvent(health)) {
		t.Error("creation and deletion of a coordination Lease are events")
	}
	if pred.Create(createEvent(mk("kube-scheduler", "x", nil, time.Unix(1, 0)))) {
		t.Error("an unrelated Lease must be ignored")
	}
}

func TestLeaseEventsReachOnlyTheHerdsThatUseThem(t *testing.T) {
	h := steady(t)
	other := herdCattleSet()
	other.Name = "unrelated"
	other.Status.FailoverGroup = &api.CattleFailoverGroupStatus{Name: "billing-app"}
	other.Status.Sites = []api.CattleSiteStatus{{ClusterName: "spoke-9"}}
	if err := h.c.Create(ctx, other); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int{
		"primary-dc-orders-app": 1, // the herd's own group
		"primary-dc-pg-docs":    1, // a group it depends on
		"dc-health-spoke-2":     1, // one of its sites
		"primary-dc-billing":    0,
		"dc-health-spoke-9":     1, // the other herd's site only
	} {
		got := h.r.leaseToCattleSets(ctx, newCoordLease(name, "x", nil, time.Now()))
		if len(got) != want {
			t.Errorf("%s reaches %d herds, want %d: %v", name, len(got), want, got)
		}
	}
}

func TestNextWake(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	soon := LeaseObservation{Found: true, Fresh: true, Expires: now.Add(10 * time.Second)}
	far := LeaseObservation{Found: true, Fresh: true, Expires: now.Add(time.Hour)}
	if got := nextWake(now, []LeaseObservation{far, soon}); got != 11*time.Second {
		t.Errorf("the earliest expiry wins (plus a second): %v", got)
	}
	if got := nextWake(now, []LeaseObservation{far}); got != maxRequeue {
		t.Errorf("far expiries are capped: %v", got)
	}
	if got := nextWake(now, nil); got != maxRequeue {
		t.Errorf("nothing to wait for: %v", got)
	}
	if got := nextWake(now, []LeaseObservation{{Found: true, Expires: now.Add(-time.Minute)}}); got != maxRequeue {
		t.Errorf("an already expired Lease must not spin the loop: %v", got)
	}
	if got := nextWake(now, []LeaseObservation{{Found: true, Fresh: true, Expires: now.Add(-500 * time.Millisecond)}}); got < minRequeue {
		t.Errorf("never faster than the floor: %v", got)
	}
}

var (
	_ = ptr.To[int32]
	_ = apiworkv1.GroupName
)
