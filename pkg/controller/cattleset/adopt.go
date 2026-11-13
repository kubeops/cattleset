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
	"sort"

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Enabling DR is a spec flip (distributed false to true) with adoption, never a
// pod restart. The local cluster's existing Deployment (same name, namespace,
// identity labels) becomes the local Member site's rendered output: the renderer
// copies the identical spec core, so the server-side apply produces no
// pod-template diff (the revision hash is unchanged, asserted in tests). The
// other Members are seeded normally. Until every site reports ready the herd
// stays in the incumbent-active posture regardless of mode, so there is no
// traffic gap during enablement (true to false is rejected by the webhook).

// applyAdoption keeps the incumbent site active with full replicas while a
// distribution flip is still enabling (not every site ready), regardless of the
// declared mode. Once all sites are ready the incumbent drops to its declared
// posture. Pure and unit-testable.
func applyAdoption(desired map[string]SiteDesired, incumbent string, replicas int32, enabling bool) {
	if !enabling || incumbent == "" {
		return
	}
	sd, ok := desired[incumbent]
	if !ok || sd.Role != petsetapi.DCRoleMember {
		return
	}
	sd.Posture = api.CattlePostureActive
	sd.Replicas = replicas
	desired[incumbent] = sd
}

// adoptionState decides, for a distributed herd, which site is the adoption
// incumbent and whether enablement is still in progress. Enablement is in
// progress while any Member site has fewer ready replicas than its pre-adoption
// desired count (a seeded standby not yet up).
func (r *Reconciler) adoptionState(ctx context.Context, cs *api.CattleSet, in PostureInput, desired map[string]SiteDesired) (string, bool) {
	incumbent := r.incumbentSite(cs, in)

	enabling := false
	for _, sd := range desired {
		if sd.Role != petsetapi.DCRoleMember {
			continue
		}
		fb := r.readSiteFeedback(ctx, cs, sd.ClusterName)
		if fb.ready < sd.Replicas {
			enabling = true
			break
		}
	}
	return incumbent, enabling
}

// incumbentSite is the annotated adopt cluster, else the local cluster when it is
// a Member, else the Lease holder, else the first Member by name.
func (r *Reconciler) incumbentSite(cs *api.CattleSet, in PostureInput) string {
	if c := cs.Annotations[api.AdoptClusterAnnotation]; c != "" {
		return c
	}
	// The existing Deployment is in the cluster this controller runs in. When that
	// cluster is not a Member (the stretched hub is the Arbiter DC and runs no
	// workloads) there is nothing to adopt: the Members are seeded and the hub-local
	// objects are retired once one of them serves (retireHubLocal).
	if r.LocalClusterName != "" {
		for _, rule := range in.Rules {
			if rule.ClusterName == r.LocalClusterName && effectiveRole(rule.Role) == petsetapi.DCRoleMember {
				return r.LocalClusterName
			}
		}
		return ""
	}
	if in.Holder != "" {
		return in.Holder
	}
	members := make([]string, 0, len(in.Rules))
	for _, rule := range in.Rules {
		if effectiveRole(rule.Role) == petsetapi.DCRoleMember {
			members = append(members, rule.ClusterName)
		}
	}
	if len(members) == 0 {
		return ""
	}
	sort.Strings(members)
	return members[0]
}

// retireHubLocal removes the Deployment, Service and PodDisruptionBudget a
// non-distributed herd left in the hub's own cluster, once a Member site is
// serving. It only applies when the hub is known not to be a Member (the
// documented stretched topology, where the hub is the Arbiter DC): there nothing
// adopts the hub-local objects, and without this they keep running outside any
// posture. It never touches an object this CattleSet does not control.
func (r *Reconciler) retireHubLocal(ctx context.Context, cs *api.CattleSet, members map[string]bool, memberServing bool) error {
	if r.LocalClusterName == "" || members[r.LocalClusterName] || !memberServing {
		return nil
	}
	var objs []client.Object
	objs = append(objs, &apps.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: cs.Namespace, Name: cs.Name}})
	objs = append(objs, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: cs.Namespace, Name: cs.Name}})
	if cs.HasTraffic() {
		objs = append(objs, &core.Service{ObjectMeta: metav1.ObjectMeta{Namespace: cs.Namespace, Name: cs.Spec.Traffic.ServiceName}})
	}
	for _, o := range objs {
		if err := r.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(o, cs) {
			continue
		}
		if err := r.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		r.event(cs, "Normal", "HubLocalRetired", "removed "+o.GetName()+" from the hub cluster, which is not a Member")
	}
	return nil
}
