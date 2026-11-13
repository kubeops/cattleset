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
	"encoding/json"
	"testing"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	"kubeops.dev/cattleset/pkg/controller/gate"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	apps "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// applyShim makes the fake client usable for the controller's server-side-apply
// writes. A real apiserver merges by field ownership; the shim does the part the
// tests rely on: an apply creates or replaces the object, and an apply of a
// FailoverGroup member list replaces this controller's entries.
type applyShim struct {
	client.Client
}

func (a *applyShim) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if patch.Type() != types.ApplyPatchType {
		return a.Client.Patch(ctx, obj, patch, opts...)
	}
	existing := obj.DeepCopyObject().(client.Object)
	err := a.Client.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		obj.SetResourceVersion("")
		return a.Client.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return a.Client.Update(ctx, obj)
}

func (a *applyShim) Status() client.SubResourceWriter {
	return &statusShim{SubResourceWriter: a.Client.Status(), c: a.Client}
}

type statusShim struct {
	client.SubResourceWriter
	c client.Client
}

// Patch implements the one apply the controller does on a status: its entry in a
// FailoverGroup's members. The entry replaces every CattleSet entry (the tests use
// one herd); an empty list removes them.
func (s *statusShim) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if patch.Type() != types.ApplyPatchType {
		return s.SubResourceWriter.Patch(ctx, obj, patch, opts...)
	}
	u := obj.(*unstructured.Unstructured)
	fg := &petsetapi.FailoverGroup{}
	if err := s.c.Get(ctx, client.ObjectKey{Name: u.GetName()}, fg); err != nil {
		return err
	}
	raw, _, _ := unstructured.NestedSlice(u.Object, "status", "members")
	var keep []petsetapi.FailoverGroupMember
	for _, m := range fg.Status.Members {
		if m.APIGroup != api.GroupName || m.Kind != api.ResourceKindCattleSet {
			keep = append(keep, m)
		}
	}
	for _, e := range raw {
		b, _ := json.Marshal(e)
		var m petsetapi.FailoverGroupMember
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		keep = append(keep, m)
	}
	fg.Status.Members = keep
	return s.c.Status().Update(ctx, fg)
}

// fakeCoord is a coordination plane the test controls.
type fakeCoord struct{ leases map[string]LeaseObservation }

func (f *fakeCoord) Lease(_ context.Context, name string) LeaseObservation {
	if o, ok := f.leases[name]; ok {
		return o
	}
	return LeaseObservation{}
}

// harness is a hub with two Member spokes and an Arbiter hub, an application and
// the database it depends on.
type harness struct {
	t     *testing.T
	c     client.Client
	r     *Reconciler
	coord *fakeCoord
	rec   *record.FakeRecorder
	now   time.Time
}

func harnessScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{core.AddToScheme, apps.AddToScheme, policyv1.AddToScheme, api.AddToScheme, petsetapi.AddToScheme, apiworkv1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func orderingObjects(appActive, dbActive string) []client.Object {
	self := petsetapi.FailoverGroupMember{APIGroup: api.GroupName, Kind: api.ResourceKindCattleSet, Namespace: "orders", Name: "orders-api"}
	return []client.Object{
		&petsetapi.PlacementPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "orders-geo"},
			Spec: petsetapi.PlacementPolicySpec{ClusterSpreadConstraint: &petsetapi.ClusterSpreadConstraint{
				DistributionRules: []petsetapi.DistributionRule{
					{ClusterName: "spoke-1", Role: petsetapi.DCRoleMember},
					{ClusterName: "spoke-2", Role: petsetapi.DCRoleMember},
					{ClusterName: "hub", Role: petsetapi.DCRoleArbiter},
				},
				FailoverPolicy: &petsetapi.FailoverPolicy{FailoverGroupRef: &core.LocalObjectReference{Name: "orders-app"}},
			}},
		},
		&petsetapi.FailoverGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "orders-app"},
			Spec:       petsetapi.FailoverGroupSpec{DependsOn: []string{"pg-docs"}},
			Status:     petsetapi.FailoverGroupStatus{ActiveDC: appActive, Phase: petsetapi.FailoverGroupPhaseSteady, Members: []petsetapi.FailoverGroupMember{self}},
		},
		&petsetapi.FailoverGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-docs"},
			Status: petsetapi.FailoverGroupStatus{ActiveDC: dbActive, Phase: petsetapi.FailoverGroupPhaseSteady, Members: []petsetapi.FailoverGroupMember{
				{APIGroup: "kubedb.com", Kind: "Postgres", Namespace: "demo", Name: "pg-docs", DC: dbActive, Ready: true},
			}},
		},
	}
}

func herdCattleSet() *api.CattleSet {
	cs := &api.CattleSet{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-api", Namespace: "orders", Generation: 1},
		Spec: api.CattleSetSpec{
			Distributed: true,
			Replicas:    ptr.To[int32](6),
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "orders-api"}},
			Template: core.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "orders-api"}},
				Spec:       core.PodSpec{Containers: []core.Container{{Name: "api", Image: "harbor.local/orders/api:1"}}},
			},
			PodPlacementPolicy: &core.LocalObjectReference{Name: "orders-geo"},
			Topology:           &api.CattleTopology{Mode: api.CattleModeAuto, StandbyReplicas: ptr.To[int32](3)},
			RolloutOrder:       api.CattleRolloutStandbyFirst,
			Traffic: &api.CattleTraffic{
				ServiceName: "orders-api",
				Ports:       []core.ServicePort{{Name: "http", Port: 80}},
			},
		},
	}
	return cs
}

// newHarness builds the hub. appActive is the FailoverGroup's activeDC for the
// application, dbActive the database's.
func newHarness(t *testing.T, appActive, dbActive string, extra ...client.Object) *harness {
	t.Helper()
	objs := append(orderingObjects(appActive, dbActive), herdCattleSet())
	objs = append(objs, extra...)
	c := &applyShim{Client: fake.NewClientBuilder().WithScheme(harnessScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&api.CattleSet{}, &petsetapi.FailoverGroup{}, &apiworkv1.ManifestWork{}).Build()}
	h := &harness{
		t: t, c: c,
		coord: &fakeCoord{leases: map[string]LeaseObservation{}},
		rec:   record.NewFakeRecorder(100),
		now:   time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
	}
	h.r = &Reconciler{Client: c, Reader: c, Scheme: c.Scheme(), Recorder: h.rec, Coord: h.coord, Now: func() time.Time { return h.now }}
	return h
}

// leaseHeld sets a primary Lease the tests treat as freshly renewed.
func (h *harness) leaseHeld(name, holder string) {
	h.coord.leases[name] = LeaseObservation{Found: true, Fresh: true, Holder: holder, Expires: h.now.Add(45 * time.Second)}
}

func (h *harness) dcHealthy(dcs ...string) {
	for _, dc := range dcs {
		h.coord.leases[healthLeaseName(dc)] = LeaseObservation{Found: true, Fresh: true, Holder: dc, Expires: h.now.Add(45 * time.Second)}
	}
}

func (h *harness) dcDead(dc string) {
	h.coord.leases[healthLeaseName(dc)] = LeaseObservation{Found: true, Fresh: false, Holder: dc, Expires: h.now.Add(-time.Minute)}
}

func (h *harness) reconcile() ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "orders", Name: "orders-api"}})
	if err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (h *harness) cs() *api.CattleSet {
	h.t.Helper()
	cs := &api.CattleSet{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "orders", Name: "orders-api"}, cs); err != nil {
		h.t.Fatal(err)
	}
	return cs
}

func (h *harness) mw(cluster, name string) *apiworkv1.ManifestWork {
	h.t.Helper()
	mw := &apiworkv1.ManifestWork{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: cluster, Name: name}, mw); err != nil {
		h.t.Fatalf("ManifestWork %s/%s: %v", cluster, name, err)
	}
	return mw
}

func (h *harness) hasMW(cluster, name string) bool {
	err := h.c.Get(context.Background(), types.NamespacedName{Namespace: cluster, Name: name}, &apiworkv1.ManifestWork{})
	return err == nil
}

func (h *harness) group(name string) *petsetapi.FailoverGroup {
	h.t.Helper()
	g := &petsetapi.FailoverGroup{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: name}, g); err != nil {
		h.t.Fatal(err)
	}
	return g
}

// member returns the herd's entry in its FailoverGroup, nil when absent.
func (h *harness) member() *petsetapi.FailoverGroupMember {
	for _, m := range h.group("orders-app").Status.Members {
		if m.APIGroup == api.GroupName && m.Kind == api.ResourceKindCattleSet {
			m := m
			return &m
		}
	}
	return nil
}

func (h *harness) setDepActive(dc string) {
	g := h.group("pg-docs")
	g.Status.ActiveDC = dc
	if err := h.c.Status().Update(context.Background(), g); err != nil {
		h.t.Fatal(err)
	}
}

// siteState is what a spoke reports back through its ManifestWork.
type siteState struct {
	available int32
	admission string // the gate agent's admission annotation ("" for none)
	beat      time.Time
	applied   bool
}

// siteUp sets a site's ManifestWork status as the OCM work agent would after the
// spoke applied it.
func (h *harness) siteUp(cluster string, st siteState) {
	h.t.Helper()
	mw := h.mw(cluster, manifestWorkName(h.cs()))
	conds := []metav1.Condition{
		{Type: apiworkv1.WorkApplied, Status: metav1.ConditionTrue, Reason: "Applied", LastTransitionTime: metav1.Now()},
		{Type: apiworkv1.WorkAvailable, Status: metav1.ConditionTrue, Reason: "Available", LastTransitionTime: metav1.Now()},
	}
	i := func(n string, v int64) apiworkv1.FeedbackValue {
		return apiworkv1.FeedbackValue{Name: n, Value: apiworkv1.FieldValue{Type: apiworkv1.Integer, Integer: ptr.To(v)}}
	}
	s := func(n, v string) apiworkv1.FeedbackValue {
		return apiworkv1.FeedbackValue{Name: n, Value: apiworkv1.FieldValue{Type: apiworkv1.String, String: ptr.To(v)}}
	}
	vals := []apiworkv1.FeedbackValue{i(feedbackReady, int64(st.available)), i(feedbackUpdated, int64(st.available)), i(feedbackAvailable, int64(st.available))}
	if st.admission != "" {
		vals = append(vals, s(feedbackAdmission, st.admission), s(feedbackHeartbeat, st.beat.UTC().Format(time.RFC3339)))
	}
	mw.Status.Conditions = conds
	mw.Status.ResourceStatus.Manifests = []apiworkv1.ManifestCondition{{StatusFeedbacks: apiworkv1.StatusFeedbackResult{Values: vals}}}
	if err := h.c.Status().Update(context.Background(), mw); err != nil {
		h.t.Fatal(err)
	}
	if st.applied {
		dmw := h.mw(cluster, depsWorkName(h.cs()))
		dmw.Status.Conditions = conds[:1]
		if err := h.c.Status().Update(context.Background(), dmw); err != nil {
			h.t.Fatal(err)
		}
	}
}

// manifestsOf decodes the manifests of a ManifestWork, whether the client handed
// them back as objects or as raw JSON (the fake client round-trips through JSON).
func manifestsOf(t *testing.T, mw *apiworkv1.ManifestWork) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	for _, m := range mw.Spec.Workload.Manifests {
		switch {
		case m.Raw != nil:
			u := &unstructured.Unstructured{}
			if err := u.UnmarshalJSON(m.Raw); err != nil {
				t.Fatal(err)
			}
			out = append(out, u)
		case m.Object != nil:
			u, ok := m.Object.(*unstructured.Unstructured)
			if !ok {
				t.Fatalf("unexpected manifest object %T", m.Object)
			}
			out = append(out, u)
		}
	}
	return out
}

// gateOf decodes the gate annotation of the Service rendered for a site.
func (h *harness) gateOf(cluster string) (gate.GateSpec, *core.Service) {
	h.t.Helper()
	for _, u := range manifestsOf(h.t, h.mw(cluster, manifestWorkName(h.cs()))) {
		if u.GetKind() != "Service" {
			continue
		}
		svc := &core.Service{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, svc); err != nil {
			h.t.Fatal(err)
		}
		raw, present := svc.Annotations[api.GateAnnotation]
		return gate.ParseSpec(raw, present), svc
	}
	h.t.Fatalf("no Service in the ManifestWork for %s", cluster)
	return gate.GateSpec{}, nil
}

// desired returns the replica count the controller set for a site.
func (h *harness) desired(cluster string) int32 {
	h.t.Helper()
	dep, ok := deploymentFromManifestWork(h.mw(cluster, manifestWorkName(h.cs())))
	if !ok {
		h.t.Fatalf("no Deployment for %s", cluster)
	}
	return *dep.Spec.Replicas
}

func (h *harness) cond(t string) *metav1.Condition {
	for _, c := range h.cs().Status.Conditions {
		if c.Type == t {
			c := c
			return &c
		}
	}
	return nil
}

// --- Lease event helpers ---

type coordLease = coordinationv1.Lease

func newCoordLease(name, holder string, ann map[string]string, renewed time.Time) *coordinationv1.Lease {
	mt := metav1.NewMicroTime(renewed)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: api.CoordNamespace, Annotations: ann},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder), RenewTime: &mt, LeaseDurationSeconds: ptr.To[int32](45)},
	}
}

func updateEvent(o, n *coordinationv1.Lease) event.TypedUpdateEvent[*coordinationv1.Lease] {
	return event.TypedUpdateEvent[*coordinationv1.Lease]{ObjectOld: o, ObjectNew: n}
}

func createEvent(o *coordinationv1.Lease) event.TypedCreateEvent[*coordinationv1.Lease] {
	return event.TypedCreateEvent[*coordinationv1.Lease]{Object: o}
}

func deleteEvent(o *coordinationv1.Lease) event.TypedDeleteEvent[*coordinationv1.Lease] {
	return event.TypedDeleteEvent[*coordinationv1.Lease]{Object: o}
}
