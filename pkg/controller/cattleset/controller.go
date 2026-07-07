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
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Reconciler realizes a CattleSet. In non-distributed mode (this file) it owns
// one local Deployment (plus the traffic Service and HTTPRoute if set) and
// behaves exactly like a Deployment. The distributed path is added in C4.
//
// The reconciler does not fork the Deployment controller: it renders and owns
// plain apps/v1 Deployments, and stock Kubernetes reconciles their pods.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=cattlesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=cattlesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=cattlesets/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	cs := &api.CattleSet{}
	if err := r.Get(ctx, req.NamespacedName, cs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if cs.IsDistributed() {
		// Distributed rendering (posture engine, ManifestWorks) lands in C4.
		log.V(4).Info("distributed CattleSet: skipped by the non-distributed reconciler")
		return ctrl.Result{}, nil
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
// and Services it owns.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.CattleSet{}).
		Owns(&apps.Deployment{}).
		Owns(&core.Service{}).
		Named("cattleset").
		Complete(r)
}
