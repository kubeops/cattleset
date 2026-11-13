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

// Package gate implements the CattleSet spoke gate agent. It runs local to a
// site and holds no hub client at all: a hub outage never blocks a legitimate
// admission nor forces a wrong one. Everything it knows is the gate spec the hub
// rendered into the Service annotation and the local objects it reads: the scope
// marker ConfigMaps dc-failover/<lease> that the DC's dr-controlplane agent
// projects, the database's local client Service endpoints, and (as a fallback)
// the database pods.
//
// It is fail closed while it runs: anything it cannot read means unadmitted. It
// is not a fence. If no agent is running, the last admission state persists; data
// safety never depends on it (the database fences itself, and the database's
// local Service is empty in a DC that is not active).
package gate

import (
	"context"
	"fmt"
	"sync"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// DefaultResync is how often a gated Service is re-evaluated. Marker staleness
	// is a matter of time passing, not of an event, so a periodic pass is required.
	DefaultResync = 5 * time.Second
	// DefaultHeartbeatEvery is how often the admission heartbeat annotation is
	// rewritten when nothing else changed. It is much longer than the resync so a
	// steady site does not write to the API server (and, through ManifestWork
	// feedback, to the hub) every few seconds.
	DefaultHeartbeatEvery = 20 * time.Second
)

// Agent is the spoke gate controller.
type Agent struct {
	client.Client
	// Reader is an uncached reader, used for the pod lookups (drain label and the
	// C3 fallback) so the agent does not hold a cluster-wide pod cache.
	Reader   client.Reader
	Recorder record.EventRecorder

	// DC is this spoke's name: the OCM managed cluster name, the same string the
	// marker's activeDC carries.
	DC string

	Resync         time.Duration
	HeartbeatEvery time.Duration
	// Now is injectable for tests.
	Now func() time.Time

	mu    sync.Mutex
	grace map[types.NamespacedName]*Grace
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) resync() time.Duration {
	if a.Resync > 0 {
		return a.Resync
	}
	return DefaultResync
}

func (a *Agent) heartbeatEvery() time.Duration {
	if a.HeartbeatEvery > 0 {
		return a.HeartbeatEvery
	}
	return DefaultHeartbeatEvery
}

func (a *Agent) graceFor(key types.NamespacedName) *Grace {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grace == nil {
		a.grace = map[types.NamespacedName]*Grace{}
	}
	g, ok := a.grace[key]
	if !ok {
		g = &Grace{}
		a.grace[key] = g
	}
	return g
}

func (a *Agent) forget(key types.NamespacedName) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.grace, key)
}

// managed reports whether the agent owns this Service's admission: it carries the
// CattleSet identity, and either the admission selector term (unadmitted) or a
// gate annotation (admitted, the term removed by the agent).
func managed(svc *core.Service) bool {
	if _, ok := svc.Labels[api.CattleSetNameLabel]; !ok {
		return false
	}
	if _, ok := svc.Spec.Selector[api.AdmittedSelectorKey]; ok {
		return true
	}
	_, ok := svc.Annotations[api.GateAnnotation]
	return ok
}

func (a *Agent) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	svc := &core.Service{}
	if err := a.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			a.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !managed(svc) {
		// An ungated Service (an ActiveActive herd with no dependencies, or a
		// non-distributed herd): the controller renders it admitted, the agent
		// leaves it alone.
		a.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	raw, present := svc.Annotations[api.GateAnnotation]
	spec := ParseSpec(raw, present)
	now := a.now()
	_, hasTerm := svc.Spec.Selector[api.AdmittedSelectorKey]
	wasAdmitted := !hasTerm

	obs, app := a.observe(ctx, spec)
	d := Evaluate(spec, obs, app, a.DC, wasAdmitted, a.graceFor(req.NamespacedName), now)

	changed, err := a.apply(ctx, svc, d, spec, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		a.record(svc, d)
		log.Info("gate flip", "service", req.NamespacedName, "admitted", d.Admit, "reason", d.Reason)
	}

	next := a.resync()
	if d.RecheckIn > 0 && d.RecheckIn < next {
		next = d.RecheckIn
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

// apply writes the decision: the selector term, the admission and heartbeat
// annotations, in one Service patch, and then the drain label on the pods. It
// reports whether admission flipped.
func (a *Agent) apply(ctx context.Context, svc *core.Service, d Decision, spec GateSpec, now time.Time) (bool, error) {
	_, hasTerm := svc.Spec.Selector[api.AdmittedSelectorKey]
	flipped := d.Admit == hasTerm // admitted wants no term; revoked wants the term

	want := svc.DeepCopy()
	if want.Spec.Selector == nil {
		want.Spec.Selector = map[string]string{}
	}
	if d.Admit {
		delete(want.Spec.Selector, api.AdmittedSelectorKey)
	} else {
		want.Spec.Selector[api.AdmittedSelectorKey] = api.AdmittedSelectorValue
	}
	if want.Annotations == nil {
		want.Annotations = map[string]string{}
	}
	reason := d.Reason
	if d.Admit {
		reason = ReasonAdmitted
	}
	reasonChanged := want.Annotations[api.AdmissionAnnotation] != reason
	want.Annotations[api.AdmissionAnnotation] = reason

	beatDue := true
	if prev, err := time.Parse(time.RFC3339, svc.Annotations[api.AdmissionHeartbeatAnnotation]); err == nil {
		beatDue = now.Sub(prev) >= a.heartbeatEvery()
	}
	if flipped || reasonChanged || beatDue {
		want.Annotations[api.AdmissionHeartbeatAnnotation] = now.UTC().Format(time.RFC3339)
		if err := a.Patch(ctx, want, client.MergeFrom(svc), client.FieldOwner(api.GateFieldManager)); err != nil {
			return false, err
		}
		if spec.DrainPodLabel {
			if err := a.labelPods(ctx, want, d.Admit); err != nil {
				klog.ErrorS(err, "labeling pods", "service", client.ObjectKeyFromObject(svc))
			}
		}
	}
	return flipped, nil
}

// labelPods sets serving=true|false on the herd's pods in this site. A pod
// created between two labeling passes carries no label until the next one, so an
// absent label means "unknown", never "serving".
func (a *Agent) labelPods(ctx context.Context, svc *core.Service, serving bool) error {
	sel := map[string]string{}
	for k, v := range svc.Spec.Selector {
		if k != api.AdmittedSelectorKey {
			sel[k] = v
		}
	}
	if len(sel) == 0 {
		return nil
	}
	rd := a.Reader
	if rd == nil {
		rd = a.Client
	}
	pods := &core.PodList{}
	if err := rd.List(ctx, pods, client.InNamespace(svc.Namespace), client.MatchingLabels(sel)); err != nil {
		return err
	}
	value := fmt.Sprintf("%t", serving)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Labels[api.ServingLabel] == value {
			continue
		}
		orig := p.DeepCopy()
		if p.Labels == nil {
			p.Labels = map[string]string{}
		}
		p.Labels[api.ServingLabel] = value
		if err := a.Patch(ctx, p, client.MergeFrom(orig), client.FieldOwner(api.GateFieldManager)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (a *Agent) record(svc *core.Service, d Decision) {
	reason, result := "SiteRevoked", "revoked"
	if d.Admit {
		reason, result = "SiteAdmitted", "admitted"
	}
	gateFlipsTotal.WithLabelValues(svc.Namespace, svc.Name, result).Inc()
	if a.Recorder == nil {
		return
	}
	a.Recorder.Event(svc, core.EventTypeNormal, reason, "admission: "+d.Reason)
}

// observe reads, for every tier (or the app lease), the local facts AdmitTier
// needs. A read that fails is reported as the absence of the fact, which can only
// close the gate.
func (a *Agent) observe(ctx context.Context, spec GateSpec) ([]TierObservation, TierObservation) {
	var app TierObservation
	switch spec.Mode {
	case GateModeAppMarker:
		app.Marker = a.readMarker(ctx, spec.AppLease)
		app.OverrideActive = a.overrideActive(ctx, spec.AppLease)
		return nil, app
	case GateModeTiers:
		obs := make([]TierObservation, len(spec.Tiers))
		for i, t := range spec.Tiers {
			obs[i] = a.observeTier(ctx, t)
		}
		return obs, app
	}
	return nil, app
}

func (a *Agent) observeTier(ctx context.Context, t GateTier) TierObservation {
	var o TierObservation
	if t.Camp != CampLeaderlessGeoNative {
		o.Marker = a.readMarker(ctx, t.Lease)
		o.OverrideActive = a.overrideActive(ctx, t.Lease)
	}
	ready := a.serviceHasReadyEndpoint(ctx, t)
	if t.Camp == CampLeaderlessGeoNative {
		o.AnyInstanceReady = ready || a.podReady(ctx, t, false)
		return o
	}
	o.PrimaryReady = ready || a.podReady(ctx, t, true)
	return o
}

func (a *Agent) readMarker(ctx context.Context, lease string) Marker {
	cm := &core.ConfigMap{}
	if err := a.Get(ctx, client.ObjectKey{Namespace: api.CoordNamespace, Name: lease}, cm); err != nil {
		return Marker{}
	}
	return ParseMarker(cm.Data)
}

func (a *Agent) overrideActive(ctx context.Context, lease string) bool {
	cm := &core.ConfigMap{}
	return a.Get(ctx, client.ObjectKey{Namespace: api.CoordNamespace, Name: lease + api.OverrideSuffix}, cm) == nil
}

// serviceHasReadyEndpoint is check C3's primary signal: the database's local
// client Service has at least one ready endpoint in this cluster.
func (a *Agent) serviceHasReadyEndpoint(ctx context.Context, t GateTier) bool {
	slices := &discoveryv1.EndpointSliceList{}
	if err := a.List(ctx, slices, client.InNamespace(t.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: t.Service}); err != nil {
		return false
	}
	for i := range slices.Items {
		for _, ep := range slices.Items[i].Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				return true
			}
		}
	}
	return false
}

// podReady is the C3 fallback: a Ready pod of the database. When primaryOnly it
// must also carry kubedb.com/role=primary. Without an instance selector there is
// nothing to look up and the fallback says no.
func (a *Agent) podReady(ctx context.Context, t GateTier, primaryOnly bool) bool {
	if len(t.InstanceSelector) == 0 {
		return false
	}
	rd := a.Reader
	if rd == nil {
		rd = a.Client
	}
	pods := &core.PodList{}
	if err := rd.List(ctx, pods, client.InNamespace(t.Namespace), client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(t.InstanceSelector)}); err != nil {
		return false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if primaryOnly && p.Labels[api.DBRoleLabel] != api.DBRolePrimaryValue {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == core.PodReady && c.Status == core.ConditionTrue && p.DeletionTimestamp == nil {
				return true
			}
		}
	}
	return false
}

// ParseMarker reads the marker ConfigMap's data. An unparsable renewTime leaves
// Renew zero, which is stale.
func ParseMarker(data map[string]string) Marker {
	m := Marker{Present: true, ActiveDC: data[api.MarkerKeyActiveDC], Quiesce: data[api.MarkerKeyQuiesce]}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, data[api.MarkerKeyRenew]); err == nil {
			m.Renew = t
			break
		}
	}
	return m
}

// SetupWithManager wires the agent. Services are the primary resource; a marker
// ConfigMap or a database EndpointSlice change enqueues only the Services whose
// gate refers to it.
func (a *Agent) SetupWithManager(mgr ctrl.Manager) error {
	hasIdentity := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		_, ok := obj.GetLabels()[api.CattleSetNameLabel]
		return ok
	})
	markers := predicate.NewPredicateFuncs(func(obj client.Object) bool { return obj.GetNamespace() == api.CoordNamespace })
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.Service{}, builder.WithPredicates(hasIdentity)).
		Watches(&core.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(a.markerToServices), builder.WithPredicates(markers)).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(a.sliceToServices)).
		Named("cattleset-gate-agent").
		Complete(a)
}

// gatedServices lists the managed Services whose gate satisfies match.
func (a *Agent) gatedServices(ctx context.Context, match func(GateSpec) bool) []reconcile.Request {
	list := &core.ServiceList{}
	if err := a.List(ctx, list, client.HasLabels{api.CattleSetNameLabel}); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		svc := &list.Items[i]
		if !managed(svc) {
			continue
		}
		raw, present := svc.Annotations[api.GateAnnotation]
		if match(ParseSpec(raw, present)) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
		}
	}
	return reqs
}

func (a *Agent) markerToServices(ctx context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetName()
	return a.gatedServices(ctx, func(s GateSpec) bool {
		if s.AppLease != "" && (name == s.AppLease || name == s.AppLease+api.OverrideSuffix) {
			return true
		}
		for _, t := range s.Tiers {
			if name == t.Lease || name == t.Lease+api.OverrideSuffix {
				return true
			}
		}
		return false
	})
}

func (a *Agent) sliceToServices(ctx context.Context, obj client.Object) []reconcile.Request {
	svcName := obj.GetLabels()[discoveryv1.LabelServiceName]
	ns := obj.GetNamespace()
	if svcName == "" {
		return nil
	}
	return a.gatedServices(ctx, func(s GateSpec) bool {
		for _, t := range s.Tiers {
			if t.Namespace == ns && t.Service == svcName {
				return true
			}
		}
		return false
	})
}
