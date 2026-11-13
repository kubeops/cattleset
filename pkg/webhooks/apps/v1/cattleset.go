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
	"context"
	"fmt"

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// SetupCattleSetWebhookWithManager registers the webhook for CattleSet in the manager.
func SetupCattleSetWebhookWithManager(mgr ctrl.Manager, arbiterHub bool) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&api.CattleSet{}).
		WithValidator(&CattleSetCustomWebhook{DefaultClient: mgr.GetClient(), ArbiterHub: arbiterHub}).
		WithDefaulter(&CattleSetCustomWebhook{DefaultClient: mgr.GetClient(), ArbiterHub: arbiterHub}).
		Complete()
}

// +kubebuilder:object:generate=false
type CattleSetCustomWebhook struct {
	DefaultClient client.Client
	// ArbiterHub says the cluster the webhook runs in is an Arbiter DC and holds no
	// workloads.
	ArbiterHub bool
}

var cattlesetlog = logf.Log.WithName("cattleset-resource")

//+kubebuilder:webhook:path=/mutate-apps-k8s-appscode-com-v1-cattleset,mutating=true,failurePolicy=fail,sideEffects=None,groups=apps.k8s.appscode.com,resources=cattlesets,verbs=create;update,versions=v1,name=mcattleset.kb.io,admissionReviewVersions=v1
//+kubebuilder:webhook:path=/validate-apps-k8s-appscode-com-v1-cattleset,mutating=false,failurePolicy=fail,sideEffects=None,groups=apps.k8s.appscode.com,resources=cattlesets,verbs=create;update,versions=v1,name=vcattleset.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &CattleSetCustomWebhook{}

// ApplyDefaults applies default values to a CattleSet. It is called from the
// mutating webhook and is safe to call from the controller for consistent
// defaulting. Extension defaults apply only when distributed is true, since
// defaulting a forbidden extension field on a non-distributed object would trip
// the webhook's own validation.
func ApplyDefaults(cs *api.CattleSet) {
	// The upstream Deployment trio, always.
	if cs.Spec.Replicas == nil {
		cs.Spec.Replicas = ptr.To[int32](1)
	}
	if cs.Spec.RevisionHistoryLimit == nil {
		cs.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	}
	if cs.Spec.ProgressDeadlineSeconds == nil {
		cs.Spec.ProgressDeadlineSeconds = ptr.To[int32](600)
	}

	if !cs.Spec.Distributed {
		return
	}
	// Topology is optional for a distributed herd: the mode then defaults to Auto,
	// which derives the posture from the data tiers it depends on.
	if cs.Spec.Topology == nil {
		cs.Spec.Topology = &api.CattleTopology{}
	}
	if cs.Spec.Topology.Mode == "" {
		cs.Spec.Topology.Mode = api.CattleModeAuto
	}
	if cs.Spec.Topology.StandbyReplicas == nil {
		cs.Spec.Topology.StandbyReplicas = ptr.To(*cs.Spec.Replicas)
	}
	if cs.Spec.Topology.SurgeOnFailover == nil {
		v := intstr.FromString("100%")
		cs.Spec.Topology.SurgeOnFailover = &v
	}
	if cs.Spec.RolloutOrder == "" {
		// Auto resolves to ActivePassive for every database that has a single
		// writer, which is the case that matters; the order always puts the serving
		// site last anyway.
		if cs.Spec.Topology.Mode == api.CattleModeActiveActive {
			cs.Spec.RolloutOrder = api.CattleRolloutOneSiteAtATime
		} else {
			cs.Spec.RolloutOrder = api.CattleRolloutStandbyFirst
		}
	}
}

func (w *CattleSetCustomWebhook) Default(_ context.Context, obj runtime.Object) error {
	cs, ok := obj.(*api.CattleSet)
	if !ok {
		return fmt.Errorf("expected a CattleSet but got a %T", obj)
	}
	ApplyDefaults(cs)
	return nil
}

var _ webhook.CustomValidator = &CattleSetCustomWebhook{}

func (w *CattleSetCustomWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	cs, ok := obj.(*api.CattleSet)
	if !ok {
		return nil, fmt.Errorf("expected a CattleSet but got a %T", obj)
	}
	refs := w.resolveRefs(ctx, cs)
	errs := ValidateCattleSet(cs, refs)
	return WarnCattleSet(cs, refs, w.ArbiterHub), errs.ToAggregate()
}

func (w *CattleSetCustomWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldCS, ok := oldObj.(*api.CattleSet)
	if !ok {
		return nil, fmt.Errorf("expected a CattleSet but got a %T", oldObj)
	}
	newCS, ok := newObj.(*api.CattleSet)
	if !ok {
		return nil, fmt.Errorf("expected a CattleSet but got a %T", newObj)
	}
	refs := w.resolveRefs(ctx, newCS)
	errs := ValidateCattleSetUpdate(oldCS, newCS)
	errs = append(errs, ValidateCattleSet(newCS, refs)...)
	return WarnCattleSet(newCS, refs, w.ArbiterHub), errs.ToAggregate()
}

func (w *CattleSetCustomWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// resolveRefs reads the PlacementPolicy the herd references. A read failure
// leaves the relevant field nil: the validator degrades to the checks that need no cluster state
// (a dangling reference is not itself rejected).
func (w *CattleSetCustomWebhook) resolveRefs(ctx context.Context, cs *api.CattleSet) resolvedRefs {
	var refs resolvedRefs
	if w.DefaultClient == nil {
		return refs
	}

	if cs.Spec.PodPlacementPolicy != nil {
		pp := &petsetapi.PlacementPolicy{}
		if err := w.DefaultClient.Get(ctx, client.ObjectKey{Name: cs.Spec.PodPlacementPolicy.Name}, pp); err == nil {
			refs.pp = pp
		} else if !apierrors.IsNotFound(err) {
			cattlesetlog.Error(err, "resolving PlacementPolicy", "name", cs.Spec.PodPlacementPolicy.Name)
		}
	}

	if refs.pp != nil {
		if group := failoverGroupRefOf(refs.pp); group != "" {
			g := &petsetapi.FailoverGroup{}
			if err := w.DefaultClient.Get(ctx, client.ObjectKey{Name: group}, g); err == nil {
				refs.group = g
				refs.depMembers = w.resolveDependencyMembers(ctx, g)
			} else if !apierrors.IsNotFound(err) {
				cattlesetlog.Error(err, "resolving FailoverGroup", "name", group)
			}
		}
	}
	return refs
}

// resolveDependencyMembers returns the members of every group g dependsOn, or nil
// when any of them cannot be read or has not reported: nil means unknown, and an
// unknown dependency is never a reason to reject.
func (w *CattleSetCustomWebhook) resolveDependencyMembers(ctx context.Context, g *petsetapi.FailoverGroup) []petsetapi.FailoverGroupMember {
	var out []petsetapi.FailoverGroupMember
	for _, dep := range g.Spec.DependsOn {
		dg := &petsetapi.FailoverGroup{}
		if err := w.DefaultClient.Get(ctx, client.ObjectKey{Name: dep}, dg); err != nil || len(dg.Status.Members) == 0 {
			return nil
		}
		out = append(out, dg.Status.Members...)
	}
	return out
}
