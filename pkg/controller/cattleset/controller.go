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

	api "kubeops.dev/cattleset/apis/apps/v1"

	apps "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// Reconciler realizes a CattleSet. In non-distributed mode (this file) it owns
// one local Deployment (plus the traffic Service and HTTPRoute if set) and
// behaves exactly like a Deployment. The distributed path is added in C4.
//
// The reconciler does not fork the Deployment controller: it renders and owns
// plain apps/v1 Deployments, and stock Kubernetes reconciles their pods.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Holder resolves the primary DC (the Lease holder) for a distributed
	// ActivePassive herd. Nil fails closed (all standby); the Lease follower
	// (C5) supplies the real one.
	Holder PrimaryDCResolver
}

func (r *Reconciler) event(cs *api.CattleSet, eventtype, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(cs, eventtype, reason, message)
	}
}

// +kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=cattlesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=cattlesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=cattlesets/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=work.open-cluster-management.io,resources=manifestworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=placementpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	cs := &api.CattleSet{}
	if err := r.Get(ctx, req.NamespacedName, cs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if cs.IsDistributed() {
		log.V(4).Info("reconciling distributed CattleSet")
		return r.reconcileDistributed(ctx, cs)
	}

	return r.reconcileLocal(ctx, cs)
}

// reconcileLocal renders and owns the single local Deployment, its Service, and
// its HTTPRoute, then projects the Deployment's status back onto the CattleSet.
func (r *Reconciler) reconcileLocal(ctx context.Context, cs *api.CattleSet) (ctrl.Result, error) {
	dep := renderDeployment(cs)
	if err := r.apply(ctx, cs, dep); err != nil {
		return ctrl.Result{}, err
	}

	// A non-distributed herd is never gated (no gates in this mode), so its
	// Service renders admitted.
	if svc := renderService(cs, false); svc != nil {
		if err := r.apply(ctx, cs, svc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Read the owned Deployment back for status projection.
	owned := &apps.Deployment{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(dep), owned); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	projectLocalStatus(cs, owned)
	if err := r.Status().Update(ctx, cs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// apply is the single writer: it sets the CattleSet as controller owner and
// server-side-applies the object with one field manager (the single-writer
// rule). Re-applying an unchanged object produces no diff, so pods are not
// restarted when an existing same-spec Deployment is adopted by name.
func (r *Reconciler) apply(ctx context.Context, cs *api.CattleSet, obj client.Object) error {
	if err := controllerutil.SetControllerReference(cs, obj, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(api.FieldManager), client.ForceOwnership)
}

// SetupWithManager wires the reconciler to watch CattleSets and the Deployments
// and Services it owns locally, plus the ManifestWorks it renders per site
// (owned by identity label, not owner reference, since they live in the
// managed-cluster namespaces on the hub).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.CattleSet{}).
		Owns(&apps.Deployment{}).
		Owns(&core.Service{}).
		Watches(&apiworkv1.ManifestWork{}, handler.EnqueueRequestsFromMapFunc(manifestWorkToCattleSet)).
		Watches(&coordinationv1.Lease{}, handler.EnqueueRequestsFromMapFunc(r.leaseToCattleSets)).
		Named("cattleset").
		Complete(r)
}

// leaseToCattleSets fans a primary-DC or health Lease change out to every
// distributed CattleSet, which recomputes posture from the new holder. Enqueuing
// all distributed herds keeps the mapping simple and the reconcile idempotent;
// a herd not on that trigger recomputes to the same posture.
func (r *Reconciler) leaseToCattleSets(ctx context.Context, _ client.Object) []ctrl.Request {
	list := &api.CattleSetList{}
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		cs := &list.Items[i]
		if !cs.IsDistributed() {
			continue
		}
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: cs.Namespace, Name: cs.Name}})
	}
	return reqs
}

// manifestWorkToCattleSet maps a ManifestWork back to its owning CattleSet using
// the identity labels the controller stamped on it.
func manifestWorkToCattleSet(_ context.Context, obj client.Object) []ctrl.Request {
	labels := obj.GetLabels()
	name := labels[api.CattleSetNameLabel]
	namespace := labels[api.CattleSetNamespaceLabel]
	if name == "" || namespace == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}
