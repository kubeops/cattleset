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

package gate

import (
	"context"
	"testing"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// spoke is a fake spoke cluster plus the agent under test. Time only moves when
// the test moves it.
type spoke struct {
	t   *testing.T
	c   client.Client
	a   *Agent
	now time.Time
}

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := core.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newSpoke(t *testing.T, objs ...client.Object) *spoke {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	sp := &spoke{t: t, c: c, now: t0}
	sp.a = sp.newAgent()
	return sp
}

func (s *spoke) newAgent() *Agent {
	return &Agent{Client: s.c, Reader: s.c, DC: here, Now: func() time.Time { return s.now }}
}

func (s *spoke) reconcile() ctrl.Result {
	s.t.Helper()
	res, err := s.a.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "orders", Name: "orders-api"}})
	if err != nil {
		s.t.Fatal(err)
	}
	return res
}

func (s *spoke) svc() *core.Service {
	s.t.Helper()
	svc := &core.Service{}
	if err := s.c.Get(context.Background(), types.NamespacedName{Namespace: "orders", Name: "orders-api"}, svc); err != nil {
		s.t.Fatal(err)
	}
	return svc
}

func (s *spoke) admitted() bool {
	_, has := s.svc().Spec.Selector[api.AdmittedSelectorKey]
	return !has
}

func (s *spoke) reason() string { return s.svc().Annotations[api.AdmissionAnnotation] }

func (s *spoke) advance(d time.Duration) { s.now = s.now.Add(d) }

// setMarker writes (or rewrites) the scope marker as the local dr-controlplane
// agent would: renewed now.
func (s *spoke) setMarker(lease, activeDC, quiesce string) {
	s.t.Helper()
	cm := &core.ConfigMap{}
	key := types.NamespacedName{Namespace: api.CoordNamespace, Name: lease}
	data := map[string]string{"activeDC": activeDC, "renewTime": s.now.UTC().Format(time.RFC3339)}
	if quiesce != "" {
		data["quiesce"] = quiesce
	}
	if err := s.c.Get(context.Background(), key, cm); err != nil {
		cm = &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Data: data}
		if err := s.c.Create(context.Background(), cm); err != nil {
			s.t.Fatal(err)
		}
		return
	}
	cm.Data = data
	if err := s.c.Update(context.Background(), cm); err != nil {
		s.t.Fatal(err)
	}
}

func (s *spoke) setPrimaryEndpoint(ready bool) {
	s.t.Helper()
	key := types.NamespacedName{Namespace: "demo", Name: "pg-docs-abc"}
	es := &discoveryv1.EndpointSlice{}
	if err := s.c.Get(context.Background(), key, es); err != nil {
		es = &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{discoveryv1.LabelServiceName: "pg-docs"}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(ready)}}},
		}
		if err := s.c.Create(context.Background(), es); err != nil {
			s.t.Fatal(err)
		}
		return
	}
	es.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(ready)}}}
	if err := s.c.Update(context.Background(), es); err != nil {
		s.t.Fatal(err)
	}
}

func gateAnnotation(t *testing.T, spec GateSpec) string {
	t.Helper()
	raw, err := spec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func herdSpec(grace int32) GateSpec {
	tier := pgTier()
	tier.InstanceSelector = map[string]string{"app.kubernetes.io/instance": "pg-docs"}
	return GateSpec{Version: SpecVersion, Mode: GateModeTiers, Tiers: []GateTier{tier}, PrimaryLossGraceSeconds: grace, DrainPodLabel: true}
}

// herdService is the Service as the hub renders it: identity labels, the gate
// annotation, and the admission term in the selector, so it starts unadmitted.
func herdService(t *testing.T, spec GateSpec) *core.Service {
	return &core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "orders",
			Name:        "orders-api",
			Labels:      map[string]string{api.CattleSetNameLabel: "orders-api", api.CattleSetNamespaceLabel: "orders"},
			Annotations: map[string]string{api.GateAnnotation: gateAnnotation(t, spec)},
		},
		Spec: core.ServiceSpec{Selector: map[string]string{"app": "orders-api", api.AdmittedSelectorKey: api.AdmittedSelectorValue}},
	}
}

func herdPod(name string) *core.Pod {
	return &core.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "orders", Name: name, Labels: map[string]string{"app": "orders-api"}}}
}

func (s *spoke) podLabel(name string) string {
	s.t.Helper()
	p := &core.Pod{}
	if err := s.c.Get(context.Background(), types.NamespacedName{Namespace: "orders", Name: name}, p); err != nil {
		s.t.Fatal(err)
	}
	return p.Labels[api.ServingLabel]
}

func TestAgentAdmitsWhenEverythingHolds(t *testing.T) {
	s := newSpoke(t, herdService(t, herdSpec(15)), herdPod("api-0"))
	s.setMarker("primary-dc-pg-docs", here, "")
	s.setPrimaryEndpoint(true)

	res := s.reconcile()
	if !s.admitted() || s.reason() != ReasonAdmitted {
		t.Fatalf("expected admitted, got selector=%v reason=%q", s.svc().Spec.Selector, s.reason())
	}
	if _, ok := s.svc().Annotations[api.AdmissionHeartbeatAnnotation]; !ok {
		t.Fatal("the heartbeat must be written on a flip")
	}
	if s.svc().Spec.Selector["app"] != "orders-api" {
		t.Fatalf("the real selector must survive: %v", s.svc().Spec.Selector)
	}
	if got := s.podLabel("api-0"); got != "true" {
		t.Fatalf("pod serving label = %q, want true", got)
	}
	if res.RequeueAfter != DefaultResync {
		t.Fatalf("must re-poll every resync (marker staleness is time, not an event), got %v", res.RequeueAfter)
	}
}

func TestAgentStaysClosedUntilThePrimaryIsHere(t *testing.T) {
	s := newSpoke(t, herdService(t, herdSpec(15)))
	s.setMarker("primary-dc-pg-docs", here, "")
	s.reconcile()
	if s.admitted() || s.reason() != ReasonNoPrimaryHere {
		t.Fatalf("marker alone is not enough (the Lease moves about 40 s before the database is writable): admitted=%v reason=%q", s.admitted(), s.reason())
	}
	s.setPrimaryEndpoint(true)
	s.reconcile()
	if !s.admitted() {
		t.Fatal("admission is immediate once the primary is here")
	}
}

func TestAgentDoesNotAdmitANonActiveDC(t *testing.T) {
	s := newSpoke(t, herdService(t, herdSpec(15)))
	s.setMarker("primary-dc-pg-docs", "spoke-2", "")
	s.setPrimaryEndpoint(true) // even an endpoint here does not help: the label outlives the marker
	s.reconcile()
	if s.admitted() || s.reason() != ReasonNotActiveHere {
		t.Fatalf("admitted=%v reason=%q", s.admitted(), s.reason())
	}
}

// The marker going stale while the agent runs revokes at once (the DC lost the
// coordination plane), even though the endpoint is still there.
func TestAgentRevokesWhenTheMarkerGoesStaleMidRun(t *testing.T) {
	s := newSpoke(t, herdService(t, herdSpec(15)), herdPod("api-0"))
	s.setMarker("primary-dc-pg-docs", here, "")
	s.setPrimaryEndpoint(true)
	s.reconcile()
	if !s.admitted() {
		t.Fatal("setup: should be admitted")
	}

	// The agent keeps reconciling; the marker is never renewed again.
	for i := 0; i < 5; i++ {
		s.advance(5 * time.Second)
		s.reconcile()
	}
	if !s.admitted() {
		t.Fatalf("25 s old marker is still fresh, got reason %q", s.reason())
	}
	s.advance(10 * time.Second) // 35 s
	s.reconcile()
	if s.admitted() || s.reason() != ReasonMarkerStale {
		t.Fatalf("a stale marker must revoke at once: admitted=%v reason=%q", s.admitted(), s.reason())
	}
	if got := s.podLabel("api-0"); got != "false" {
		t.Fatalf("pod serving label = %q, want false after a revoke", got)
	}
}

func TestAgentRevokesOnQuiesceImmediately(t *testing.T) {
	s := newSpoke(t, herdService(t, herdSpec(15)))
	s.setMarker("primary-dc-pg-docs", here, "")
	s.setPrimaryEndpoint(true)
	s.reconcile()
	s.advance(time.Second)
	s.setMarker("primary-dc-pg-docs", here, here)
	s.reconcile()
	if s.admitted() || s.reason() != ReasonQuiescing {
		t.Fatalf("a planned switchover freezes writes: admitted=%v reason=%q", s.admitted(), s.reason())
	}
}

// An intra-DC primary re-election (4 to 6 s) must not flap admission.
func TestAgentRidesOutAnIntraDCPrimaryChange(t *testing.T) {
	s := newSpoke(t, herdService(t, herdSpec(15)), herdPod("api-0"))
	s.setMarker("primary-dc-pg-docs", here, "")
	s.setPrimaryEndpoint(true)
	s.reconcile()
	beat := s.svc().Annotations[api.AdmissionHeartbeatAnnotation]

	s.advance(time.Second)
	s.setMarker("primary-dc-pg-docs", here, "")
	s.setPrimaryEndpoint(false) // the primary pod dies
	res := s.reconcile()
	if !s.admitted() {
		t.Fatal("must hold admission when only the primary is lost")
	}
	if res.RequeueAfter != 15*time.Second && res.RequeueAfter != DefaultResync {
		t.Fatalf("unexpected requeue %v", res.RequeueAfter)
	}

	s.advance(5 * time.Second)
	s.setMarker("primary-dc-pg-docs", here, "")
	s.reconcile()
	if !s.admitted() {
		t.Fatal("still inside the 15 s grace at t+5")
	}
	if got := s.podLabel("api-0"); got != "true" {
		t.Fatalf("the drain label must not flap during the grace, got %q", got)
	}

	s.advance(time.Second)
	s.setMarker("primary-dc-pg-docs", here, "")
	s.setPrimaryEndpoint(true) // a new primary within 6 s
	s.reconcile()
	if !s.admitted() || s.reason() != ReasonAdmitted {
		t.Fatalf("recovered inside the grace: admitted=%v reason=%q", s.admitted(), s.reason())
	}
	_ = beat
}

func TestAgentRevokesWhenThePrimaryDoesNotReturn(t *testing.T) {
	s := newSpoke(t, herdService(t, herdSpec(15)))
	s.setMarker("primary-dc-pg-docs", here, "")
	s.setPrimaryEndpoint(true)
	s.reconcile()

	s.setPrimaryEndpoint(false)
	for i := 0; i < 3; i++ { // t+0, t+5, t+10
		s.reconcile()
		s.advance(5 * time.Second)
		s.setMarker("primary-dc-pg-docs", here, "")
	}
	if !s.admitted() {
		t.Fatal("grace not over at t+15 minus an instant")
	}
	s.reconcile() // t+15
	if s.admitted() || s.reason() != ReasonNoPrimaryHere {
		t.Fatalf("grace is over: admitted=%v reason=%q", s.admitted(), s.reason())
	}
}

// A restarted agent has no memory of when the loss started. It must not revoke
// early or admit forever: the grace simply starts over.
func TestAgentRestartDuringALossRestartsTheGrace(t *testing.T) {
	s := newSpoke(t, herdService(t, herdSpec(15)))
	s.setMarker("primary-dc-pg-docs", here, "")
	s.setPrimaryEndpoint(true)
	s.reconcile()
	s.setPrimaryEndpoint(false)
	s.advance(10 * time.Second)
	s.setMarker("primary-dc-pg-docs", here, "")

	s.a = s.newAgent() // restart
	s.reconcile()
	if !s.admitted() {
		t.Fatal("a restarted agent must not revoke on the first pass of a graced loss")
	}
	s.advance(14 * time.Second)
	s.setMarker("primary-dc-pg-docs", here, "")
	s.reconcile()
	if !s.admitted() {
		t.Fatal("grace restarted at the restart, so t+14 is still inside it")
	}
	s.advance(2 * time.Second)
	s.setMarker("primary-dc-pg-docs", here, "")
	s.reconcile()
	if s.admitted() {
		t.Fatal("the restarted grace must still end")
	}
}

func TestAgentBreakGlassOverrideAdmits(t *testing.T) {
	override := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: api.CoordNamespace, Name: "primary-dc-pg-docs" + api.OverrideSuffix}}
	s := newSpoke(t, herdService(t, herdSpec(15)), override)
	// no marker at all: the hub is gone
	s.setPrimaryEndpoint(true)
	s.reconcile()
	if !s.admitted() {
		t.Fatalf("the operator forced this DC writable; the app must follow, reason=%q", s.reason())
	}
}

// Review R2: a Service with the admission term and no usable gate annotation is
// closed, never open.
func TestAgentClosedWithoutAUsableGate(t *testing.T) {
	cases := map[string]string{"missing": "", "garbage": "{", "unknown version": `{"version":9,"mode":"Open"}`}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			svc := herdService(t, herdSpec(15))
			if raw == "" {
				delete(svc.Annotations, api.GateAnnotation)
			} else {
				svc.Annotations[api.GateAnnotation] = raw
			}
			s := newSpoke(t, svc)
			s.setMarker("primary-dc-pg-docs", here, "")
			s.setPrimaryEndpoint(true)
			s.reconcile()
			if s.admitted() || s.reason() != ReasonClosed {
				t.Fatalf("admitted=%v reason=%q", s.admitted(), s.reason())
			}
		})
	}
}

func TestAgentClosesAnAdmittedServiceWhenTheGateIsUnreadable(t *testing.T) {
	svc := herdService(t, herdSpec(15))
	delete(svc.Spec.Selector, api.AdmittedSelectorKey) // currently admitted
	svc.Annotations[api.GateAnnotation] = "{"
	s := newSpoke(t, svc)
	s.reconcile()
	if s.admitted() {
		t.Fatal("an admitted Service whose gate cannot be read must be closed")
	}
}

func TestAgentLeaderlessNeedsOnlyALocalInstance(t *testing.T) {
	spec := GateSpec{Version: SpecVersion, Mode: GateModeTiers, Tiers: []GateTier{{Lease: "primary-dc-feed", Namespace: "demo", Name: "feed", Camp: CampLeaderlessGeoNative, Service: "pg-docs"}}}
	s := newSpoke(t, herdService(t, spec))
	s.reconcile()
	if s.admitted() || s.reason() != ReasonNoInstance {
		t.Fatalf("no instance here: admitted=%v reason=%q", s.admitted(), s.reason())
	}
	s.setPrimaryEndpoint(true)
	s.reconcile()
	if !s.admitted() {
		t.Fatalf("a local instance admits a leaderless tier with no marker, reason=%q", s.reason())
	}
}

func TestAgentAppMarkerMode(t *testing.T) {
	spec := GateSpec{Version: SpecVersion, Mode: GateModeAppMarker, AppLease: "primary-dc-orders-app"}
	s := newSpoke(t, herdService(t, spec))
	s.setMarker("primary-dc-orders-app", "spoke-2", "")
	s.reconcile()
	if s.admitted() {
		t.Fatal("not active here")
	}
	s.setMarker("primary-dc-orders-app", here, "")
	s.reconcile()
	if !s.admitted() {
		t.Fatalf("the app's own marker names this DC, reason=%q", s.reason())
	}
}

func TestAgentLeavesUnmanagedServicesAlone(t *testing.T) {
	svc := herdService(t, herdSpec(15))
	svc.Labels = nil // no identity
	s := newSpoke(t, svc)
	before := s.svc().ResourceVersion
	s.reconcile()
	if s.svc().ResourceVersion != before {
		t.Fatal("a Service without the CattleSet identity must not be touched")
	}

	// An ungated herd Service (no term, no annotation) is also left alone.
	svc = herdService(t, herdSpec(15))
	delete(svc.Spec.Selector, api.AdmittedSelectorKey)
	delete(svc.Annotations, api.GateAnnotation)
	s = newSpoke(t, svc)
	before = s.svc().ResourceVersion
	s.reconcile()
	if s.svc().ResourceVersion != before {
		t.Fatal("an ungated Service must not be touched")
	}
}

// A steady site must not write to the API server (and through ManifestWork
// feedback, to the hub) every resync.
func TestAgentHeartbeatRate(t *testing.T) {
	s := newSpoke(t, herdService(t, herdSpec(15)))
	s.setMarker("primary-dc-pg-docs", here, "")
	s.setPrimaryEndpoint(true)
	s.reconcile()
	rv := s.svc().ResourceVersion

	for i := 0; i < 3; i++ { // 15 s of 5 s resyncs
		s.advance(5 * time.Second)
		s.setMarker("primary-dc-pg-docs", here, "")
		s.reconcile()
	}
	if s.svc().ResourceVersion != rv {
		t.Fatal("no Service write expected inside the heartbeat interval")
	}
	s.advance(10 * time.Second) // 25 s since the first beat
	s.setMarker("primary-dc-pg-docs", here, "")
	s.reconcile()
	if s.svc().ResourceVersion == rv {
		t.Fatal("the heartbeat must be rewritten after the interval")
	}
}

func TestMappersFindOnlyTheServicesThatReferenceAnObject(t *testing.T) {
	s := newSpoke(t, herdService(t, herdSpec(15)))
	ctx := context.Background()

	marker := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: api.CoordNamespace, Name: "primary-dc-pg-docs"}}
	if reqs := s.a.markerToServices(ctx, marker); len(reqs) != 1 || reqs[0].Name != "orders-api" {
		t.Fatalf("marker mapping: %v", reqs)
	}
	override := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: api.CoordNamespace, Name: "primary-dc-pg-docs" + api.OverrideSuffix}}
	if reqs := s.a.markerToServices(ctx, override); len(reqs) != 1 {
		t.Fatalf("override mapping: %v", reqs)
	}
	other := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: api.CoordNamespace, Name: "primary-dc-unrelated"}}
	if reqs := s.a.markerToServices(ctx, other); len(reqs) != 0 {
		t.Fatalf("an unrelated marker must not enqueue: %v", reqs)
	}

	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: "x", Labels: map[string]string{discoveryv1.LabelServiceName: "pg-docs"}}}
	if reqs := s.a.sliceToServices(ctx, slice); len(reqs) != 1 {
		t.Fatalf("slice mapping: %v", reqs)
	}
	wrongNS := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "x", Labels: map[string]string{discoveryv1.LabelServiceName: "pg-docs"}}}
	if reqs := s.a.sliceToServices(ctx, wrongNS); len(reqs) != 0 {
		t.Fatalf("a same-named Service in another namespace must not enqueue: %v", reqs)
	}
}

// The strings below are a contract with dr-controlplane and pg-coordinator, which
// are copied rather than shared. If either side renames one, this must fail.
func TestCoordinationContractIsPinned(t *testing.T) {
	for got, want := range map[string]string{
		api.CoordNamespace:     "dc-failover",
		api.GlobalPrimaryLease: "primary-dc",
		api.GroupPrimaryPrefix: "primary-dc-",
		api.HealthLeasePrefix:  "dc-health-",
		api.OverrideSuffix:     "-override",
		api.MarkerKeyActiveDC:  "activeDC",
		api.MarkerKeyRenew:     "renewTime",
		api.MarkerKeyQuiesce:   "quiesce",
		api.AnnSwitchoverTo:    "dr.kubedb.com/switchover-to",
		api.AnnSwitchoverAbort: "dr.kubedb.com/switchover-abort",
		api.DBRoleLabel:        "kubedb.com/role",
		api.DBRolePrimaryValue: "primary",
	} {
		if got != want {
			t.Errorf("contract string changed: got %q, want %q", got, want)
		}
	}
}
