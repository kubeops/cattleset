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
	"sort"

	api "kubeops.dev/cattleset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// depsWorkName is the per-CattleSet, per-cluster ManifestWork that carries the
// namespace and the objects the pods need (syncResources). It is separate from
// the workload ManifestWork so one unreadable Secret cannot block a Deployment
// update, and Deployment feedback does not carry user data.
func depsWorkName(cs *api.CattleSet) string { return manifestWorkName(cs) + "-deps" }

// syncKind is one allowed syncResources kind.
type syncKind struct {
	kind     string
	resource string
	newObj   func() client.Object
}

var syncKinds = map[string]syncKind{
	"ConfigMap":      {"ConfigMap", "configmaps", func() client.Object { return &core.ConfigMap{} }},
	"Secret":         {"Secret", "secrets", func() client.Object { return &core.Secret{} }},
	"ServiceAccount": {"ServiceAccount", "serviceaccounts", func() client.Object { return &core.ServiceAccount{} }},
}

// syncRef is a validated syncResources entry.
type syncRef struct {
	Kind string
	Name string
}

func (s syncRef) String() string { return s.Kind + "/" + s.Name }

// syncRefs returns the valid, de-duplicated entries of spec.syncResources in a
// stable order. Anything outside ConfigMap, Secret and ServiceAccount is dropped
// (the webhook rejects it; the controller never copies another kind).
func syncRefs(cs *api.CattleSet) []syncRef {
	seen := map[syncRef]bool{}
	var out []syncRef
	for _, r := range cs.Spec.SyncResources {
		if _, ok := syncKinds[r.Kind]; !ok || r.Name == "" || (r.APIGroup != nil && *r.APIGroup != "") {
			continue
		}
		ref := syncRef{Kind: r.Kind, Name: r.Name}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// copyForSpoke strips everything that belongs to the hub object (identity,
// ownership, resourceVersion, managed fields) and keeps what the pods need. A
// ServiceAccount is copied by name, labels and annotations only: its token
// Secrets and secrets list are not copied.
func copyForSpoke(cs *api.CattleSet, src client.Object) (client.Object, error) {
	labels := map[string]string{}
	for k, v := range src.GetLabels() {
		labels[k] = v
	}
	for k, v := range renderedLabels(cs, nil) {
		labels[k] = v
	}
	annotations := map[string]string{}
	for k, v := range src.GetAnnotations() {
		annotations[k] = v
	}
	delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
	meta := metav1.ObjectMeta{Name: src.GetName(), Namespace: cs.Namespace, Labels: labels}
	if len(annotations) > 0 {
		meta.Annotations = annotations
	}
	switch o := src.(type) {
	case *core.ConfigMap:
		return &core.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta, Data: o.Data, BinaryData: o.BinaryData, Immutable: o.Immutable}, nil
	case *core.Secret:
		return &core.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: meta, Type: o.Type, Data: o.Data, Immutable: o.Immutable}, nil
	case *core.ServiceAccount:
		return &core.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: meta}, nil
	}
	return nil, fmt.Errorf("syncResources: unsupported kind %T", src)
}

// depsResult is the outcome of building a herd's dependency ManifestWork.
type depsResult struct {
	// Work is the ManifestWork to apply to every Member cluster (its namespace is
	// set per cluster by the caller).
	Objects []client.Object
	// Missing names the entries whose source is not on the hub. Their copies on the
	// spokes are kept.
	Missing []syncRef
}

// buildDeps reads every listed object from the hub namespace. A source that is
// not there is not deleted from the spokes: the copy already in the existing
// ManifestWork is carried forward, and the entry is reported as missing. Nothing
// is removed because of a read error. An entry removed from the list is simply
// not carried, which removes it from the spokes.
func (r *Reconciler) buildDeps(ctx context.Context, cs *api.CattleSet, existing *apiworkv1.ManifestWork) (depsResult, error) {
	var res depsResult
	res.Objects = append(res.Objects, &core.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: cs.Namespace, Labels: renderedLabels(cs, nil)},
	})
	carried := carriedManifests(existing)
	for _, ref := range syncRefs(cs) {
		k := syncKinds[ref.Kind]
		src := k.newObj()
		err := r.reader().Get(ctx, client.ObjectKey{Namespace: cs.Namespace, Name: ref.Name}, src)
		switch {
		case err == nil:
			cp, cerr := copyForSpoke(cs, src)
			if cerr != nil {
				return res, cerr
			}
			res.Objects = append(res.Objects, cp)
		case apierrors.IsNotFound(err):
			res.Missing = append(res.Missing, ref)
			if u, ok := carried[ref]; ok {
				res.Objects = append(res.Objects, u)
			}
		default:
			// A read error is neither "present" nor "gone": keep what is there.
			res.Missing = append(res.Missing, ref)
			if u, ok := carried[ref]; ok {
				res.Objects = append(res.Objects, u)
			}
			return res, err
		}
	}
	return res, nil
}

// carriedManifests indexes the synced objects already in a deps ManifestWork.
func carriedManifests(mw *apiworkv1.ManifestWork) map[syncRef]client.Object {
	out := map[syncRef]client.Object{}
	if mw == nil {
		return out
	}
	for _, m := range mw.Spec.Workload.Manifests {
		u := &unstructured.Unstructured{}
		raw := m.Raw
		if raw == nil && m.Object != nil {
			if un, ok := m.Object.(*unstructured.Unstructured); ok {
				u = un.DeepCopy()
				raw = nil
			}
		}
		if raw != nil {
			if err := u.UnmarshalJSON(raw); err != nil {
				continue
			}
		}
		if _, ok := syncKinds[u.GetKind()]; !ok {
			continue
		}
		out[syncRef{Kind: u.GetKind(), Name: u.GetName()}] = u
	}
	return out
}

// buildDepsManifestWork packs the namespace and the synced objects for one
// cluster. The Namespace is never deleted with the work (Orphan): other
// workloads may share it.
func buildDepsManifestWork(cs *api.CattleSet, cluster string, objs []client.Object) (*apiworkv1.ManifestWork, error) {
	manifests := make([]apiworkv1.Manifest, 0, len(objs))
	var configs []apiworkv1.ManifestConfigOption
	for _, obj := range objs {
		u, err := toUnstructured(obj)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, apiworkv1.Manifest{RawExtension: rawOf(u)})
		if ns, ok := obj.(*core.Namespace); ok {
			configs = append(configs, apiworkv1.ManifestConfigOption{
				ResourceIdentifier: apiworkv1.ResourceIdentifier{Group: "", Resource: "namespaces", Name: ns.Name},
			})
		}
	}
	labels := renderedLabels(cs, &cluster)
	labels[api.ManifestWorkClusterNameLabel] = cluster
	return &apiworkv1.ManifestWork{
		TypeMeta:   metav1.TypeMeta{APIVersion: "work.open-cluster-management.io/v1", Kind: "ManifestWork"},
		ObjectMeta: metav1.ObjectMeta{Name: depsWorkName(cs), Namespace: cluster, Labels: labels},
		Spec: apiworkv1.ManifestWorkSpec{
			Workload:        apiworkv1.ManifestsTemplate{Manifests: manifests},
			ManifestConfigs: configs,
			DeleteOption:    &apiworkv1.DeleteOption{PropagationPolicy: apiworkv1.DeletePropagationPolicyTypeSelectivelyOrphan, SelectivelyOrphan: &apiworkv1.SelectivelyOrphan{OrphaningRules: []apiworkv1.OrphaningRule{{Resource: "namespaces", Name: cs.Namespace}}}},
		},
	}, nil
}

// depsApplied reports whether the deps ManifestWork is applied on its cluster.
func depsApplied(mw *apiworkv1.ManifestWork) bool {
	return mw != nil && meta.IsStatusConditionTrue(mw.Status.Conditions, apiworkv1.WorkApplied)
}

// --- finalizer and pruning (review G5) ---

// ownedManifestWorks lists every ManifestWork this CattleSet owns, in any cluster
// namespace, by identity label: they cannot carry an owner reference because they
// live in the managed-cluster namespaces on the hub.
func (r *Reconciler) ownedManifestWorks(ctx context.Context, cs *api.CattleSet) ([]apiworkv1.ManifestWork, error) {
	list := &apiworkv1.ManifestWorkList{}
	if err := r.List(ctx, list, client.MatchingLabels(cs.OffsetLabels())); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// pruneManifestWorks deletes the ManifestWorks of clusters that are no longer
// Member sites, so removing a cluster from the PlacementPolicy removes the
// workload from it.
func (r *Reconciler) pruneManifestWorks(ctx context.Context, cs *api.CattleSet, members map[string]bool) error {
	owned, err := r.ownedManifestWorks(ctx, cs)
	if err != nil {
		return err
	}
	for i := range owned {
		mw := &owned[i]
		if members[mw.Namespace] {
			continue
		}
		if err := r.Delete(ctx, mw); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		r.event(cs, "Normal", "SitePruned", "removed the workload from "+mw.Namespace+", which is no longer a Member cluster")
	}
	return nil
}

// deleteManifestWorks removes every ManifestWork this CattleSet owns.
func (r *Reconciler) deleteManifestWorks(ctx context.Context, cs *api.CattleSet) error {
	return r.pruneManifestWorks(ctx, cs, nil)
}
