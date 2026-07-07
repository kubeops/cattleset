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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	kmapi "kmodules.xyz/client-go/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// SetupCattleSetWebhookWithManager registers the webhook for CattleSet in the manager.
func SetupCattleSetWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&api.CattleSet{}).
		WithValidator(&CattleSetCustomWebhook{mgr.GetClient()}).
		WithDefaulter(&CattleSetCustomWebhook{mgr.GetClient()}).
		Complete()
}

// +kubebuilder:object:generate=false
type CattleSetCustomWebhook struct {
	DefaultClient client.Client
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

	if !cs.Spec.Distributed || cs.Spec.Topology == nil {
		return
	}

	if cs.Spec.Topology.StandbyReplicas == nil {
		cs.Spec.Topology.StandbyReplicas = ptr.To(*cs.Spec.Replicas)
	}
	if cs.Spec.Topology.SurgeOnFailover == nil {
		v := intstr.FromString("100%")
		cs.Spec.Topology.SurgeOnFailover = &v
	}
	if cs.Spec.RolloutOrder == "" {
		if cs.Spec.Topology.Mode == api.CattleModeActivePassive {
			cs.Spec.RolloutOrder = api.CattleRolloutStandbyFirst
		} else {
			cs.Spec.RolloutOrder = api.CattleRolloutOneSiteAtATime
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
	return nil, errs.ToAggregate()
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
	errs := ValidateCattleSetUpdate(oldCS, newCS)
	errs = append(errs, ValidateCattleSet(newCS, w.resolveRefs(ctx, newCS))...)
	return nil, errs.ToAggregate()
}

func (w *CattleSetCustomWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// resolveRefs reads the PlacementPolicy the herd references and, when gated, the
// failover trigger each data tier follows. A read failure leaves the relevant
// field nil: the validator degrades to the checks that need no cluster state
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

	if cs.Spec.Gates != nil && len(cs.Spec.Gates.DataTiers) > 0 {
		refs.dataTierTriggers = make([]*petsetapi.FailoverTrigger, len(cs.Spec.Gates.DataTiers))
		for i, dt := range cs.Spec.Gates.DataTiers {
			refs.dataTierTriggers[i] = w.resolveDataTierTrigger(ctx, cs.Namespace, dt)
		}
	}
	return refs
}

// resolveDataTierTrigger duck-types a data tier object (a KubeDB Database or any
// object referencing a PlacementPolicy at spec.podPlacementPolicy.name) to find
// the failover trigger it follows, without taking a module dependency on kubedb.
func (w *CattleSetCustomWebhook) resolveDataTierTrigger(ctx context.Context, ns string, dt kmapi.TypedObjectReference) *petsetapi.FailoverTrigger {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   dt.APIGroup,
		Version: "v1",
		Kind:    dt.Kind,
	})
	namespace := ns
	if dt.Namespace != "" {
		namespace = dt.Namespace
	}
	if err := w.DefaultClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: dt.Name}, u); err != nil {
		return nil
	}
	ppName, found, err := unstructured.NestedString(u.Object, "spec", "podPlacementPolicy", "name")
	if err != nil || !found || ppName == "" {
		return nil
	}
	pp := &petsetapi.PlacementPolicy{}
	if err := w.DefaultClient.Get(ctx, client.ObjectKey{Name: ppName}, pp); err != nil {
		return nil
	}
	return herdTrigger(pp)
}
