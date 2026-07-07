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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PrimaryDCResolver reports the current primary DC (the Lease holderIdentity)
// and the set of Members whose per-DC health Lease is stale. The Lease follower
// (C5) implements it; a nil resolver fails closed (no holder), so an
// ActivePassive herd stays all-standby until the follower is wired.
type PrimaryDCResolver interface {
	PrimaryDC(ctx context.Context, cs *api.CattleSet) (holder string, stale map[string]bool, err error)
}

// reconcileDistributed resolves the PlacementPolicy, computes posture, renders
// one ManifestWork per Member site, applies them to the hub, and aggregates the
// sites' feedback into status.
func (r *Reconciler) reconcileDistributed(ctx context.Context, cs *api.CattleSet) (ctrl.Result, error) {
	pp, err := r.resolvePlacementPolicy(ctx, cs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pp == nil || pp.Spec.ClusterSpreadConstraint == nil {
		// The webhook rejects this at admission; if it slips through, do nothing
		// rather than render an empty herd.
		return ctrl.Result{}, nil
	}

	holder, stale := r.resolvePrimaryDC(ctx, cs)
	in := postureInput(cs, pp, holder, stale)
	desired := ComputePosture(in)

	gatedServiceName := ""
	if cs.IsGated() && cs.Spec.Traffic != nil {
		gatedServiceName = cs.Spec.Traffic.ServiceName
	}

	// Order the sites for a stable apply and status.
	sites := sortedSites(desired)

	// Rollout sequencing: hold back sites' template updates per rolloutOrder.
	// paused blocks template propagation but never posture (a failover still
	// executes on the old template).
	target := revisionHash(cs.Spec.Template)
	maxUnavailable := r.maxUnavailableOf(cs)
	plan := rolloutPlan(cs.Spec.RolloutOrder, target, maxUnavailable, r.gatherRolloutSites(ctx, cs, sites), cs.Spec.Paused)

	for _, site := range sites {
		if site.Role != petsetapi.DCRoleMember {
			continue // pods run only in Member DCs
		}
		allowUpdate := plan[site.ClusterName]
		if err := r.applySiteManifestWork(ctx, cs, site, gatedServiceName, pp, in.Mode, holder, allowUpdate); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.aggregateDistributedStatus(ctx, cs, holder, sites); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) resolvePrimaryDC(ctx context.Context, cs *api.CattleSet) (string, map[string]bool) {
	if r.Holder == nil {
		return "", nil // fail closed
	}
	holder, stale, err := r.Holder.PrimaryDC(ctx, cs)
	if err != nil {
		return "", nil // fail closed on a read error
	}
	return holder, stale
}

func (r *Reconciler) resolvePlacementPolicy(ctx context.Context, cs *api.CattleSet) (*petsetapi.PlacementPolicy, error) {
	if cs.Spec.PodPlacementPolicy == nil {
		return nil, nil
	}
	pp := &petsetapi.PlacementPolicy{}
	if err := r.Get(ctx, client.ObjectKey{Name: cs.Spec.PodPlacementPolicy.Name}, pp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return pp, nil
}

func postureInput(cs *api.CattleSet, pp *petsetapi.PlacementPolicy, holder string, stale map[string]bool) PostureInput {
	in := PostureInput{
		Replicas: valueOr(cs.Spec.Replicas, 1),
		Rules:    pp.Spec.ClusterSpreadConstraint.DistributionRules,
		Holder:   holder,
		Stale:    stale,
	}
	if cs.Spec.Topology != nil {
		in.Mode = cs.Spec.Topology.Mode
		in.StandbyReplicas = valueOr(cs.Spec.Topology.StandbyReplicas, in.Replicas)
		in.Distribution = cs.Spec.Topology.Distribution
		if cs.Spec.Topology.SurgeOnFailover != nil {
			in.Surge = *cs.Spec.Topology.SurgeOnFailover
		} else {
			in.Surge = intstr.FromString("100%")
		}
	}
	return in
}

// applySiteManifestWork renders the site's Deployment, Service, PDB, and traffic
// objects (HTTPRoute, ServiceExport, Gslb), packs them into a ManifestWork, and
// server-side-applies it to the hub with a single field manager.
func (r *Reconciler) applySiteManifestWork(ctx context.Context, cs *api.CattleSet, site SiteDesired, gatedServiceName string, pp *petsetapi.PlacementPolicy, mode api.CattleMode, holder string, allowUpdate bool) error {
	dep, err := r.siteDeployment(ctx, cs, site, allowUpdate)
	if err != nil {
		return err
	}
	objs := []client.Object{dep}

	if svc := renderService(cs, gatedServiceName != ""); svc != nil {
		objs = append(objs, svc)
	}
	if pdb := renderPDB(cs, site); pdb != nil {
		objs = append(objs, pdb)
	}
	if hr := renderHTTPRoute(cs); hr != nil {
		objs = append(objs, hr)
	}
	if se := renderServiceExport(cs, pp); se != nil {
		objs = append(objs, se)
	}
	if gslb := renderGslb(cs, mode, holder); gslb != nil {
		objs = append(objs, gslb)
	}

	mw, err := buildManifestWork(cs, site, objs, gatedServiceName)
	if err != nil {
		return err
	}
	return r.Patch(ctx, mw, client.Apply, client.FieldOwner(api.FieldManager), client.ForceOwnership)
}

// siteDeployment renders the site's Deployment. When a rollout is holding this
// site back (allowUpdate false) it keeps the pod template and revision the site
// currently runs, so the new template does not reach it yet, while the new
// replica count (posture) still applies: a failover is never held.
func (r *Reconciler) siteDeployment(ctx context.Context, cs *api.CattleSet, site SiteDesired, allowUpdate bool) (*apps.Deployment, error) {
	dep, err := renderSiteDeployment(cs, site)
	if err != nil {
		return nil, err
	}
	if allowUpdate {
		return dep, nil
	}
	mw := &apiworkv1.ManifestWork{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: site.ClusterName, Name: manifestWorkName(cs)}, mw); err != nil {
		return dep, nil // no existing site (first apply): nothing to hold
	}
	old, ok := deploymentFromManifestWork(mw)
	if !ok {
		return dep, nil
	}
	// Hold the old template and revision; keep the new replica count.
	dep.Spec.Template = old.Spec.Template
	if rev, has := old.Labels[api.RevisionLabel]; has {
		dep.Labels[api.RevisionLabel] = rev
	}
	return dep, nil
}

// gatherRolloutSites reads each Member site's current revision and rollout
// feedback from its ManifestWork, so rolloutPlan can decide which site may move.
func (r *Reconciler) gatherRolloutSites(ctx context.Context, cs *api.CattleSet, sites []SiteDesired) []rolloutSite {
	var out []rolloutSite
	for _, site := range sites {
		if site.Role != petsetapi.DCRoleMember {
			continue
		}
		rs := rolloutSite{
			ClusterName: site.ClusterName,
			Posture:     site.Posture,
			Desired:     site.Replicas,
		}
		mw := &apiworkv1.ManifestWork{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: site.ClusterName, Name: manifestWorkName(cs)}, mw); err == nil {
			if dep, ok := deploymentFromManifestWork(mw); ok {
				rs.CurrentRevision = dep.Labels[api.RevisionLabel]
			}
			fb := readFeedbackFromMW(mw)
			rs.Updated = fb.updated
			rs.Available = fb.available
		}
		out = append(out, rs)
	}
	return out
}

func (r *Reconciler) maxUnavailableOf(cs *api.CattleSet) int32 {
	if ru := cs.Spec.Strategy.RollingUpdate; ru != nil && ru.MaxUnavailable != nil {
		v, err := intstr.GetScaledValueFromIntOrPercent(ru.MaxUnavailable, int(valueOr(cs.Spec.Replicas, 1)), true)
		if err == nil {
			return int32(v)
		}
	}
	return 1
}

// aggregateDistributedStatus reads each site's ManifestWork feedback and rolls
// it up into status.sites plus the DeploymentStatus-mirror totals, then sets the
// posture conditions and the current primary DC.
func (r *Reconciler) aggregateDistributedStatus(ctx context.Context, cs *api.CattleSet, holder string, sites []SiteDesired) error {
	var siteStatuses []api.CattleSiteStatus
	var totalReplicas, totalReady, totalUpdated, totalAvailable int32
	converged := true

	for _, site := range sites {
		ss := api.CattleSiteStatus{
			ClusterName: site.ClusterName,
			Role:        site.Role,
			Posture:     site.Posture,
			Desired:     site.Replicas,
		}
		if site.Role == petsetapi.DCRoleMember {
			fb := r.readSiteFeedback(ctx, cs, site.ClusterName)
			ss.Ready = fb.ready
			ss.Updated = fb.updated
			ss.Gated = fb.gated
			ss.Revision = fb.revision
			totalReplicas += site.Replicas
			totalReady += fb.ready
			totalUpdated += fb.updated
			totalAvailable += fb.available
			if fb.ready < site.Replicas {
				converged = false
			}
		}
		siteStatuses = append(siteStatuses, ss)
	}

	cs.Status.ObservedGeneration = cs.Generation
	cs.Status.Selector = metav1.FormatLabelSelector(cs.Spec.Selector)
	cs.Status.CurrentPrimaryDC = holder
	cs.Status.Sites = siteStatuses
	cs.Status.Replicas = totalReplicas
	cs.Status.ReadyReplicas = totalReady
	cs.Status.UpdatedReplicas = totalUpdated
	cs.Status.AvailableReplicas = totalAvailable
	if totalReplicas >= totalAvailable {
		cs.Status.UnavailableReplicas = totalReplicas - totalAvailable
	}

	setBoolCondition(cs, api.ConditionPostureConverged, converged, "PostureConverged", "SitesConverging")
	setBoolCondition(cs, api.ConditionAvailable, totalAvailable > 0, "MinimumReplicasAvailable", "NoAvailableReplicas")
	setBoolCondition(cs, api.ConditionProgressing, !converged, "Progressing", "Converged")

	return r.Status().Update(ctx, cs)
}

type siteFeedback struct {
	ready, updated, available int32
	gated                     bool
	revision                  string
}

// readSiteFeedback pulls a site's rollout aggregates from its ManifestWork
// status feedback. A missing ManifestWork (a dead or not-yet-joined site) reads
// as zeros, which is correct: nothing is ready there.
func (r *Reconciler) readSiteFeedback(ctx context.Context, cs *api.CattleSet, cluster string) siteFeedback {
	mw := &apiworkv1.ManifestWork{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster, Name: manifestWorkName(cs)}, mw); err != nil {
		return siteFeedback{}
	}
	return readFeedbackFromMW(mw)
}

// readFeedbackFromMW parses the rollout aggregates and the applied revision from
// a ManifestWork.
func readFeedbackFromMW(mw *apiworkv1.ManifestWork) siteFeedback {
	var fb siteFeedback
	if dep, ok := deploymentFromManifestWork(mw); ok {
		fb.revision = dep.Labels[api.RevisionLabel]
	}
	for _, m := range mw.Status.ResourceStatus.Manifests {
		for _, v := range m.StatusFeedbacks.Values {
			if v.Value.Integer == nil {
				continue
			}
			switch v.Name {
			case "readyReplicas":
				fb.ready = int32(*v.Value.Integer)
			case "updatedReplicas":
				fb.updated = int32(*v.Value.Integer)
			case "availableReplicas":
				fb.available = int32(*v.Value.Integer)
			}
		}
	}
	return fb
}

func setBoolCondition(cs *api.CattleSet, condType string, ok bool, trueReason, falseReason string) {
	status := metav1.ConditionFalse
	reason := falseReason
	if ok {
		status = metav1.ConditionTrue
		reason = trueReason
	}
	meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		ObservedGeneration: cs.Generation,
	})
}

func sortedSites(desired map[string]SiteDesired) []SiteDesired {
	out := make([]SiteDesired, 0, len(desired))
	for _, sd := range desired {
		out = append(out, sd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClusterName < out[j].ClusterName })
	return out
}

func valueOr(p *int32, fallback int32) int32 {
	if p == nil {
		return fallback
	}
	return *p
}
