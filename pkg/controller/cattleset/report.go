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
	"fmt"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reportRefresh is how often an unchanged member report is rewritten, well inside
// the FailoverGroup controller's 5 minute MemberReportTTL.
const reportRefresh = 60 * time.Second

type reportState struct {
	hash string
	at   time.Time
}

// reportMember tells the herd's FailoverGroup where this herd is ready, and, when
// a planned switchover is under way, where it wants the Lease to go. The ready
// bit is MemberReady's, for the DC where the herd's own Lease is held.
func (r *Reconciler) reportMember(ctx context.Context, cs *api.CattleSet, ord Ordering, holder string, views map[string]siteView, handoffTo string, now time.Time) error {
	v, ok := views[holder]
	if !ok {
		return nil // the holder is not one of this herd's Member sites
	}
	depElsewhere := false
	for _, dg := range ord.DepGroups {
		if dg.ActiveDC != "" && dg.ActiveDC != holder {
			depElsewhere = true
		}
	}
	ready := MemberReady(MemberReadyInput{
		Healthy:             v.Healthy,
		AvailableReplicas:   v.Feedback.available,
		MinReady:            cs.Spec.Admission.MinReady(),
		Admitted:            v.Admitted,
		DependencyElsewhere: depElsewhere,
	})
	return r.applyMember(ctx, cs, ord.GroupName, holder, ready, handoffTo, now)
}

// applyMember server-side-applies this herd's entry in the FailoverGroup's status
// with its own field manager. Fields it leaves out are released, so an empty
// handoffTo withdraws the request. An unchanged report is rewritten only once a
// minute, to keep observedAt fresh without a write per reconcile.
func (r *Reconciler) applyMember(ctx context.Context, cs *api.CattleSet, group, dc string, ready bool, handoffTo string, now time.Time) error {
	key := types.NamespacedName{Namespace: cs.Namespace, Name: cs.Name}
	hash := fmt.Sprintf("%s|%t|%s|%s", dc, ready, handoffTo, group)

	r.mu.Lock()
	last, seen := r.lastReport[key]
	r.mu.Unlock()
	if seen && last.hash == hash && now.Sub(last.at) < reportRefresh {
		return nil
	}

	entry := map[string]any{
		"apiGroup":   api.GroupName,
		"kind":       api.ResourceKindCattleSet,
		"namespace":  cs.Namespace,
		"name":       cs.Name,
		"dc":         dc,
		"observedAt": now.UTC().Format(time.RFC3339),
	}
	if ready {
		entry["ready"] = true
	}
	if handoffTo != "" {
		entry["handoffTo"] = handoffTo
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": petsetapi.SchemeGroupVersion.String(),
		"kind":       petsetapi.ResourceKindFailoverGroup,
		"metadata":   map[string]any{"name": group},
		"status":     map[string]any{"members": []any{entry}},
	}}
	if err := r.Status().Patch(ctx, u, client.Apply, client.FieldOwner(api.FailoverGroupFieldManager), client.ForceOwnership); err != nil {
		return err
	}
	r.mu.Lock()
	if r.lastReport == nil {
		r.lastReport = map[types.NamespacedName]reportState{}
	}
	r.lastReport[key] = reportState{hash: hash, at: now}
	r.mu.Unlock()
	return nil
}

// forgetReport drops the throttle state of a herd that is going away.
func (r *Reconciler) forgetReport(cs *api.CattleSet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.lastReport, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Name})
}

// withdrawMember removes this herd's entry from its FailoverGroup when the herd is
// deleted, so a group is not left waiting on a member that no longer exists. It
// applies an empty member list under this controller's field manager, which
// releases the entry it owned.
func (r *Reconciler) withdrawMember(ctx context.Context, cs *api.CattleSet, group string) error {
	if group == "" {
		return nil
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": petsetapi.SchemeGroupVersion.String(),
		"kind":       petsetapi.ResourceKindFailoverGroup,
		"metadata":   map[string]any{"name": group},
		"status":     map[string]any{"members": []any{}},
	}}
	r.forgetReport(cs)
	return client.IgnoreNotFound(r.Status().Patch(ctx, u, client.Apply, client.FieldOwner(api.FailoverGroupFieldManager), client.ForceOwnership))
}

// handleSwitchover runs one pass of the planned switchover and applies the part
// of the decision that is this controller's to apply: removing the annotations on
// every exit, and recording why. Setting handoffTo on the member entry is the
// reporter's job; the plan carries its value.
func (r *Reconciler) handleSwitchover(ctx context.Context, cs *api.CattleSet, ord Ordering, appObs LeaseObservation, views map[string]siteView, members []string) SwitchoverPlan {
	target := cs.Annotations[api.AnnSwitchoverTo]
	_, abort := cs.Annotations[api.AnnSwitchoverAbort]

	var started time.Time
	if c := meta.FindStatusCondition(cs.Status.Conditions, api.ConditionSwitchoverInProgress); c != nil && c.Status == metav1.ConditionTrue {
		started = c.LastTransitionTime.Time
	}
	depActive := make([]string, 0, len(ord.DepGroups))
	for _, dg := range ord.DepGroups {
		depActive = append(depActive, dg.ActiveDC)
	}
	v := views[target]
	plan := PlanSwitchover(SwitchoverInput{
		Target:       target,
		Abort:        abort,
		Started:      started,
		Now:          r.now(),
		Members:      members,
		HasDeps:      ord.Group != nil && len(ord.Group.Spec.DependsOn) > 0,
		DepActiveDCs: depActive,
		AppHolder:    appObs.Holder,
		HolderKnown:  appObs.HolderKnown(),
		TargetReady:  v.Healthy && v.Feedback.available >= cs.Spec.Admission.MinReady(),
	})
	if !plan.ClearAnnotations {
		return plan
	}

	orig := cs.DeepCopy()
	delete(cs.Annotations, api.AnnSwitchoverTo)
	delete(cs.Annotations, api.AnnSwitchoverAbort)
	if err := r.Patch(ctx, cs, client.MergeFrom(orig)); err != nil {
		// Without the removal the group can never be asked again; try again next pass.
		r.event(orig, "Warning", "SwitchoverCleanupFailed", err.Error())
		return plan
	}
	switch plan.Step {
	case StepReject:
		r.event(cs, "Warning", "SwitchoverRejected", plan.Message)
	case StepTimeout:
		r.event(cs, "Warning", "SwitchoverTimedOut", plan.Message)
	case StepAbort:
		r.event(cs, "Normal", "SwitchoverAborted", plan.Message)
	case StepDone:
		r.event(cs, "Normal", "SwitchoverComplete", plan.Message)
	}
	return plan
}

// dataTierStalled reports whether a dependency member says its promotion is held
// by the loss budget (DCDRPromotionStalled), read from the member object on the
// hub. known is false when a member could not be read or its kind is not
// resolvable, in which case nothing is concluded.
func (r *Reconciler) dataTierStalled(ctx context.Context, ord Ordering) (stalled, known bool) {
	known = true
	for _, d := range ord.Deps {
		m := d.Member
		mapping, err := r.RESTMapper().RESTMapping(schema.GroupKind{Group: m.APIGroup, Kind: m.Kind})
		if err != nil {
			known = false
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(mapping.GroupVersionKind)
		if err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: m.Name}, u); err != nil {
			known = false
			continue
		}
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if ok && cm["type"] == "DCDRPromotionStalled" && cm["status"] == "True" {
				return true, known
			}
		}
	}
	return false, known
}
