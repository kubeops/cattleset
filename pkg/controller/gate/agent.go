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

// Package gate implements the CattleSet spoke gate agent: the write-path gate.
// It runs local to a site and is hub-independent (it holds no hub client at
// all). It admits a gated herd's Service (removes the controller-owned admission
// selector term) only while every referenced data tier reports this site as its
// promoted, writable primary, and revokes on any demotion, unknown, or absence
// (design Section 6).
package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	kmapi "kmodules.xyz/client-go/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Agent is the spoke gate controller. It watches the local rendered Services
// (by CattleSet identity label) and flips their admission term based on the
// local data tiers. It has no hub client: a hub outage never blocks a
// legitimate admission nor forces a wrong one.
type Agent struct {
	client.Client
	Recorder record.EventRecorder

	// Resync is how often a gated Service re-polls its data tiers. The tiers are
	// dynamic, per-herd GVKs the agent cannot statically watch, so promotion is
	// picked up within one resync. Defaults to 15s.
	Resync time.Duration
}

const defaultResync = 15 * time.Second

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;patch;update

func (a *Agent) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	svc := &core.Service{}
	if err := a.Get(ctx, req.NamespacedName, svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tiers, gated := gatedDataTiers(svc)
	if !gated {
		// Ungated Service (no gates): the controller renders it admitted and the
		// agent leaves it alone.
		return ctrl.Result{}, nil
	}

	states := a.resolveTierStates(ctx, svc.Namespace, tiers)
	admit := decideAdmit(states)

	changed, err := a.setAdmission(ctx, svc, admit)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		a.record(svc, admit, states)
		log.Info("gate flip", "service", req.NamespacedName, "admitted", admit, "tiers", states)
	}
	// Re-poll the tiers on a resync: they are dynamic GVKs the agent cannot
	// statically watch, so a promotion is picked up within one interval.
	return ctrl.Result{RequeueAfter: a.resync()}, nil
}

func (a *Agent) resync() time.Duration {
	if a.Resync > 0 {
		return a.Resync
	}
	return defaultResync
}

// gatedDataTiers reads the gated data tiers stamped on the Service by the
// controller. A Service without the annotation is ungated.
func gatedDataTiers(svc *core.Service) ([]kmapi.TypedObjectReference, bool) {
	raw, ok := svc.Annotations[api.GatedDataTiersAnnotation]
	if !ok || raw == "" {
		return nil, false
	}
	var tiers []kmapi.TypedObjectReference
	if err := json.Unmarshal([]byte(raw), &tiers); err != nil {
		return nil, false
	}
	return tiers, true
}

func (a *Agent) resolveTierStates(ctx context.Context, ns string, tiers []kmapi.TypedObjectReference) []tierState {
	states := make([]tierState, 0, len(tiers))
	for _, t := range tiers {
		st := tierState{Name: t.Name}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: t.APIGroup, Version: "v1", Kind: t.Kind})
		namespace := ns
		if t.Namespace != "" {
			namespace = t.Namespace
		}
		if err := a.Get(ctx, client.ObjectKey{Namespace: namespace, Name: t.Name}, u); err != nil {
			if !apierrors.IsNotFound(err) {
				klog.FromContext(ctx).Error(err, "reading data tier", "name", t.Name)
			}
			states = append(states, st) // Present=false, Promoted=false: fail closed
			continue
		}
		st.Present = true
		st.Promoted = isPromotedHere(u)
		states = append(states, st)
	}
	return states
}

// setAdmission makes the Service admitted (removes the admission term) or
// unadmitted (restores it). Admission is one atomic Service patch: the selector
// is an atomic map, so after creation the agent owns the whole selector,
// relying on the ManifestWork ignoreFields wiring (C4) so the work agent never
// reverts the flip. Returns whether the Service changed.
func (a *Agent) setAdmission(ctx context.Context, svc *core.Service, admit bool) (bool, error) {
	_, present := svc.Spec.Selector[api.AdmittedSelectorKey]
	if admit && !present {
		return false, nil // already admitted
	}
	if !admit && present {
		return false, nil // already revoked
	}

	patched := svc.DeepCopy()
	if patched.Spec.Selector == nil {
		patched.Spec.Selector = map[string]string{}
	}
	if admit {
		delete(patched.Spec.Selector, api.AdmittedSelectorKey)
	} else {
		patched.Spec.Selector[api.AdmittedSelectorKey] = api.AdmittedSelectorValue
	}
	if err := a.Patch(ctx, patched, client.MergeFrom(svc), client.FieldOwner(api.GateFieldManager)); err != nil {
		return false, err
	}
	return true, nil
}

func (a *Agent) record(svc *core.Service, admit bool, states []tierState) {
	reason := "SiteRevoked"
	result := "revoked"
	if admit {
		reason = "SiteAdmitted"
		result = "admitted"
	}
	gateFlipsTotal.WithLabelValues(svc.Namespace, svc.Name, result).Inc()
	if a.Recorder == nil {
		return
	}
	a.Recorder.Event(svc, core.EventTypeNormal, reason, tierSummary(states))
}

func tierSummary(states []tierState) string {
	parts := make([]string, 0, len(states))
	for _, s := range states {
		switch {
		case !s.Present:
			parts = append(parts, fmt.Sprintf("%s=absent", s.Name))
		case s.Promoted:
			parts = append(parts, fmt.Sprintf("%s=promoted", s.Name))
		default:
			parts = append(parts, fmt.Sprintf("%s=demoted", s.Name))
		}
	}
	return fmt.Sprintf("data tiers: %v", parts)
}

// SetupWithManager wires the agent to watch the local rendered Services (those
// carrying a CattleSet identity label).
func (a *Agent) SetupWithManager(mgr ctrl.Manager) error {
	hasIdentity := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		_, ok := obj.GetLabels()[api.CattleSetNameLabel]
		return ok
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.Service{}, builder.WithPredicates(hasIdentity)).
		Named("cattleset-gate-agent").
		Complete(a)
}
