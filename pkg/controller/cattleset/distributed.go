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
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	"kubeops.dev/cattleset/pkg/controller/gate"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	apps "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// heartbeatStaleAfter is how long a site's admission heartbeat may be silent
	// before the gate agent is considered gone: three of the agent's default
	// heartbeats (20 s).
	heartbeatStaleAfter = 60 * time.Second
	// minRequeue and maxRequeue bound the timer that re-reads Leases for expiry.
	minRequeue = 2 * time.Second
	maxRequeue = 30 * time.Second
)

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) lease(ctx context.Context, name string) LeaseObservation {
	if r.Coord == nil {
		return LeaseObservation{}
	}
	return r.Coord.Lease(ctx, name)
}

// siteView is one Member site as the hub sees it: its desired state, the
// ManifestWork feedback, and the health and admission derived from it.
type siteView struct {
	Site SiteDesired
	// Feedback is what the ManifestWork reported, zeroed when the site is not
	// healthy: a dead spoke's feedback keeps its last value forever, and must
	// never be read as ready (review R3, G7).
	Feedback siteFeedback
	Health   Health
	Healthy  bool
	// Admitted is the Service selecting endpoints: the agent said Admitted and its
	// heartbeat is fresh, or the herd has no gate and the site has pods.
	Admitted        bool
	AdmissionReason string
	HeartbeatStale  bool
	DepsApplied     bool
	ManifestWork    *apiworkv1.ManifestWork
	depsWork        *apiworkv1.ManifestWork
}

// reconcileDistributed renders one ManifestWork per Member site, decides capacity
// and admission, takes part in the FailoverGroup ordering, and projects the
// result into status.
func (r *Reconciler) reconcileDistributed(ctx context.Context, cs *api.CattleSet) (ctrl.Result, error) {
	log := klog.FromContext(ctx)
	if !cs.DeletionTimestamp.IsZero() {
		return r.finalizeDistributed(ctx, cs)
	}
	if !controllerutil.ContainsFinalizer(cs, api.CleanupFinalizer) {
		controllerutil.AddFinalizer(cs, api.CleanupFinalizer)
		if err := r.Update(ctx, cs); err != nil {
			return ctrl.Result{}, err
		}
	}

	pp, err := r.resolvePlacementPolicy(ctx, cs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pp == nil || pp.Spec.ClusterSpreadConstraint == nil {
		// The webhook rejects this at admission; if it slips through, render nothing
		// rather than an empty herd, and say so.
		setCond(cs, r.now(), api.ConditionOrderingConfigured, false, "PlacementPolicyMissing", "the referenced PlacementPolicy does not exist or has no clusterSpreadConstraint")
		return ctrl.Result{RequeueAfter: maxRequeue}, r.Status().Update(ctx, cs)
	}
	rules := pp.Spec.ClusterSpreadConstraint.DistributionRules
	members := memberClusters(rules)
	memberSet := map[string]bool{}
	for _, m := range members {
		memberSet[m] = true
	}
	now := r.now()

	// What the hub knows about ordering, and what the coordination plane says.
	groupName := groupNameFor(pp)
	ord, err := r.resolveOrdering(ctx, groupName)
	if err != nil {
		return ctrl.Result{}, err
	}
	var appObs LeaseObservation
	if groupName != "" {
		appObs = r.lease(ctx, primaryLeaseName(groupName))
	}
	var depObs []LeaseObservation
	for _, dg := range ord.DepGroups {
		if dg.Found {
			depObs = append(depObs, r.lease(ctx, primaryLeaseName(dg.Name)))
		}
	}
	health := map[string]LeaseObservation{}
	for _, m := range members {
		health[m] = r.lease(ctx, healthLeaseName(m))
	}

	// Posture and gate. The derived mode tightens by itself and never loosens.
	specChanged := cs.Status.ObservedGeneration != cs.Generation
	d := deriveGate(cs, ord, cs.Status.DerivedMode, specChanged)

	views := r.readSiteViews(ctx, cs, members, health, d.Gate != nil, groupName != "", now)

	prev := previousSites(cs)
	in := postureInput(cs, rules, d.Mode)
	in.Stale = staleMembers(health)
	hold := false
	if d.Mode == api.CattleModeActivePassive {
		admitted := map[string]bool{}
		for c, v := range views {
			admitted[c] = v.Admitted
		}
		in.Active, hold = activeSet(members, appObs, depObs, admitted, prev, ord.Group)
		in.Previous = prev
		in.Hold = hold
		if appObs.HolderKnown() {
			in.Holder = appObs.Holder
		}
	}
	desired := ComputePosture(in)

	// Distribution flip (false to true): keep the incumbent site active until
	// every site is ready, so enabling DR under load drops no endpoints.
	incumbent, enabling := r.adoptionState(ctx, cs, in, desired)
	applyAdoption(desired, incumbent, in.Replicas, enabling)

	sites := sortedSites(desired)
	for i := range sites {
		if v, ok := views[sites[i].ClusterName]; ok {
			v.Site = sites[i]
			views[sites[i].ClusterName] = v
		}
	}

	// Rollout sequencing. A paused herd, a switchover or an unsettled failover
	// holds template propagation; capacity and admission are never held.
	switchoverTarget := cs.Annotations[api.AnnSwitchoverTo]
	holdRollout := switchoverTarget != "" || (ord.Group != nil && ord.Group.Status.Phase != "" && ord.Group.Status.Phase != petsetapi.FailoverGroupPhaseSteady)
	maxUnavailable := r.maxUnavailableOf(cs)
	plan := rolloutPlan(cs.Spec.RolloutOrder, maxUnavailable, r.gatherRolloutSites(cs, sites, views), cs.Spec.Paused || holdRollout)

	servingDC := firstAdmitted(views, members)
	var depsErr error
	missing := map[syncRef]bool{}
	for _, site := range sites {
		if site.Role != petsetapi.DCRoleMember {
			continue
		}
		v := views[site.ClusterName]
		// Dependencies first: the workload cannot start without them.
		if res, err := r.applyDeps(ctx, cs, site.ClusterName, v.depsWork); err != nil {
			depsErr = err
		} else {
			for _, m := range res.Missing {
				missing[m] = true
			}
		}
		if err := r.applySiteManifestWork(ctx, cs, site, d.Gate, d.Mode, servingDC, plan[site.ClusterName]); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.pruneManifestWorks(ctx, cs, memberSet); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.retireHubLocal(ctx, cs, memberSet, servingDC != ""); err != nil {
		log.Error(err, "retiring the hub-local workload")
	}

	// The planned switchover, and what this herd tells its FailoverGroup.
	swPlan := r.handleSwitchover(ctx, cs, ord, appObs, views, members)
	if groupName != "" && ord.Group != nil && appObs.HolderKnown() {
		if err := r.reportMember(ctx, cs, ord, appObs.Holder, views, swPlan.HandoffTo, now); err != nil {
			log.Error(err, "reporting to the FailoverGroup")
		}
	}

	stalled, stalledKnown := r.dataTierStalled(ctx, ord)
	r.writeStatus(cs, statusFacts{
		pp: pp, rules: rules, ord: ord, derived: d, appObs: appObs, health: health,
		views: views, sites: sites, hold: hold, swPlan: swPlan,
		missing: missing, depsErr: depsErr, now: now,
		stalled: stalled, stalledKnown: stalledKnown,
	})
	if err := r.Status().Update(ctx, cs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: nextWake(now, append(append([]LeaseObservation{appObs}, depObs...), healthList(health)...))}, nil
}

// memberClusters returns the Member cluster names (a Witness rule is an Arbiter).
func memberClusters(rules []petsetapi.DistributionRule) []string {
	var out []string
	for _, r := range rules {
		if effectiveRole(r.Role) == petsetapi.DCRoleMember {
			out = append(out, r.ClusterName)
		}
	}
	sort.Strings(out)
	return out
}

func healthList(h map[string]LeaseObservation) []LeaseObservation {
	out := make([]LeaseObservation, 0, len(h))
	for _, o := range h {
		out = append(out, o)
	}
	return out
}

// staleMembers marks Members whose health Lease is dead or unreadable: their
// share is absorbed by survivors. (Surge only adds capacity, so over-reporting on
// a read error is the cautious side.) A Member with no health Lease at all is a
// cluster without DC-DR coordination and is treated as healthy.
func staleMembers(health map[string]LeaseObservation) map[string]bool {
	stale := map[string]bool{}
	for c, o := range health {
		if h := o.Health(); h == HealthDead || h == HealthUnknown {
			stale[c] = true
		}
	}
	if len(stale) == 0 {
		return nil
	}
	return stale
}

// nextWake is when the reconcile must run again for lack of any event: just after
// the earliest Lease expires. A plain Lease renewal produces no event (it would
// reconcile every herd every few seconds), so expiry is detected by this timer.
func nextWake(now time.Time, obs []LeaseObservation) time.Duration {
	next := maxRequeue
	for _, o := range obs {
		if !o.Found || o.Expires.IsZero() {
			continue
		}
		if d := o.Expires.Sub(now) + time.Second; d > 0 && d < next {
			next = d
		}
	}
	if next < minRequeue {
		next = minRequeue
	}
	return next
}

func firstAdmitted(views map[string]siteView, members []string) string {
	for _, m := range members {
		if views[m].Admitted {
			return m
		}
	}
	return ""
}

func previousSites(cs *api.CattleSet) map[string]SiteDesired {
	out := map[string]SiteDesired{}
	for _, s := range cs.Status.Sites {
		out[s.ClusterName] = SiteDesired{ClusterName: s.ClusterName, Role: s.Role, Posture: s.Posture, Replicas: s.Desired}
	}
	return out
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

func postureInput(cs *api.CattleSet, rules []petsetapi.DistributionRule, mode api.CattleMode) PostureInput {
	in := PostureInput{
		Mode:     mode,
		Replicas: valueOr(cs.Spec.Replicas, 1),
		Rules:    rules,
		Surge:    intstr.FromString("100%"),
	}
	in.StandbyReplicas = in.Replicas
	if t := cs.Spec.Topology; t != nil {
		in.StandbyReplicas = valueOr(t.StandbyReplicas, in.Replicas)
		in.Distribution = t.Distribution
		if t.SurgeOnFailover != nil {
			in.Surge = *t.SurgeOnFailover
		}
	}
	return in
}

// applySiteManifestWork renders the site's Deployment, Service, PDB and traffic
// objects, packs them into a ManifestWork, and server-side-applies it to the hub
// with a single field manager.
func (r *Reconciler) applySiteManifestWork(ctx context.Context, cs *api.CattleSet, site SiteDesired, spec *gate.GateSpec, mode api.CattleMode, servingDC string, allowUpdate bool) error {
	dep, err := r.siteDeployment(ctx, cs, site, allowUpdate)
	if err != nil {
		return err
	}
	objs := []client.Object{dep}

	svc, err := renderService(cs, spec)
	if err != nil {
		return err
	}
	if svc != nil {
		objs = append(objs, svc)
	}
	if pdb := renderPDB(cs, site); pdb != nil {
		objs = append(objs, pdb)
	}
	if hr := renderHTTPRoute(cs); hr != nil {
		objs = append(objs, hr)
	}
	if gslb := renderGslb(cs, mode, servingDC); gslb != nil {
		objs = append(objs, gslb)
	}

	mw, err := buildManifestWork(cs, site, objs, spec != nil)
	if err != nil {
		return err
	}
	return r.Patch(ctx, mw, client.Apply, client.FieldOwner(api.FieldManager), client.ForceOwnership)
}

// applyDeps applies the namespace and the synced objects to one cluster. A failure
// to read a source never removes what the spoke already has.
func (r *Reconciler) applyDeps(ctx context.Context, cs *api.CattleSet, cluster string, existing *apiworkv1.ManifestWork) (depsResult, error) {
	res, err := r.buildDeps(ctx, cs, existing)
	if err != nil {
		return res, err
	}
	mw, err := buildDepsManifestWork(cs, cluster, res.Objects)
	if err != nil {
		return res, err
	}
	return res, r.Patch(ctx, mw, client.Apply, client.FieldOwner(api.FieldManager), client.ForceOwnership)
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

func (r *Reconciler) maxUnavailableOf(cs *api.CattleSet) int32 {
	if ru := cs.Spec.Strategy.RollingUpdate; ru != nil && ru.MaxUnavailable != nil {
		v, err := intstr.GetScaledValueFromIntOrPercent(ru.MaxUnavailable, int(valueOr(cs.Spec.Replicas, 1)), true)
		if err == nil {
			return int32(v)
		}
	}
	return 1
}

// readSiteViews reads each Member's two ManifestWorks and derives health and
// admission. A site is healthy only if its DC's health Lease is alive, and its
// ManifestWork is Applied and Available. An unhealthy site reports zero: feedback
// from a spoke that stopped answering keeps its last values, and counting them
// would report a dead DC as ready.
func (r *Reconciler) readSiteViews(ctx context.Context, cs *api.CattleSet, members []string, health map[string]LeaseObservation, gated, hasGroup bool, now time.Time) map[string]siteView {
	out := make(map[string]siteView, len(members))
	for _, c := range members {
		v := siteView{Health: health[c].Health()}
		mw := &apiworkv1.ManifestWork{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: c, Name: manifestWorkName(cs)}, mw); err == nil {
			v.ManifestWork = mw
		}
		dmw := &apiworkv1.ManifestWork{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: c, Name: depsWorkName(cs)}, dmw); err == nil {
			v.depsWork = dmw
			v.DepsApplied = depsApplied(dmw)
		}

		// A cluster with no health Lease at all is one without DC-DR coordination;
		// that is only acceptable for a herd that has no group to follow.
		healthOK := v.Health == HealthAlive || (v.Health == HealthAbsent && !hasGroup)
		applied := mw.Name != "" && meta.IsStatusConditionTrue(mw.Status.Conditions, apiworkv1.WorkApplied)
		available := mw.Name != "" && meta.IsStatusConditionTrue(mw.Status.Conditions, apiworkv1.WorkAvailable)
		v.Healthy = v.ManifestWork != nil && applied && available && healthOK

		if v.ManifestWork != nil {
			fb := readFeedbackFromMW(v.ManifestWork)
			if v.Healthy {
				v.Feedback = fb
			} else {
				v.Feedback = siteFeedback{revision: fb.revision}
			}
		}

		switch {
		case !v.Healthy:
			v.AdmissionReason = "Unknown"
		case !gated:
			v.Admitted = v.Feedback.available > 0
			if v.Admitted {
				v.AdmissionReason = gate.ReasonAdmitted
			} else {
				v.AdmissionReason = "NoPods"
			}
		default:
			fresh := !v.Feedback.heartbeat.IsZero() && now.Sub(v.Feedback.heartbeat) <= heartbeatStaleAfter
			v.HeartbeatStale = !fresh
			switch {
			case !fresh:
				v.AdmissionReason = "Unknown"
			case v.Feedback.admission == gate.ReasonAdmitted:
				v.Admitted, v.AdmissionReason = true, gate.ReasonAdmitted
			default:
				v.AdmissionReason = nonEmpty(v.Feedback.admission, "Unknown")
			}
		}
		out[c] = v
	}
	return out
}

// gatherRolloutSites builds the rollout view of every Member site.
func (r *Reconciler) gatherRolloutSites(cs *api.CattleSet, sites []SiteDesired, views map[string]siteView) []rolloutSite {
	var out []rolloutSite
	for _, site := range sites {
		if site.Role != petsetapi.DCRoleMember {
			continue
		}
		v := views[site.ClusterName]
		rs := rolloutSite{
			ClusterName:    site.ClusterName,
			Posture:        site.Posture,
			Desired:        site.Replicas,
			Admitted:       v.Admitted,
			TargetRevision: siteRevision(cs, site.ClusterName),
			Updated:        v.Feedback.updated,
			Available:      v.Feedback.available,
		}
		if v.ManifestWork != nil {
			if dep, ok := deploymentFromManifestWork(v.ManifestWork); ok {
				rs.CurrentRevision = dep.Labels[api.RevisionLabel]
			}
		}
		out = append(out, rs)
	}
	return out
}

// siteFeedback is what a site's ManifestWork reports.
type siteFeedback struct {
	ready, updated, available int32
	revision                  string
	// admission and heartbeat are the gate agent's scalar annotations, projected
	// back by a JSONPath feedback rule on the Service.
	admission string
	heartbeat time.Time
}

// readSiteFeedback pulls a site's rollout aggregates from its ManifestWork
// status feedback. A missing ManifestWork reads as zeros.
func (r *Reconciler) readSiteFeedback(ctx context.Context, cs *api.CattleSet, cluster string) siteFeedback {
	mw := &apiworkv1.ManifestWork{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster, Name: manifestWorkName(cs)}, mw); err != nil {
		return siteFeedback{}
	}
	return readFeedbackFromMW(mw)
}

// readFeedbackFromMW parses the rollout aggregates, the applied revision and the
// gate agent's admission annotations from a ManifestWork.
func readFeedbackFromMW(mw *apiworkv1.ManifestWork) siteFeedback {
	var fb siteFeedback
	if dep, ok := deploymentFromManifestWork(mw); ok {
		fb.revision = dep.Labels[api.RevisionLabel]
	}
	for _, m := range mw.Status.ResourceStatus.Manifests {
		for _, v := range m.StatusFeedbacks.Values {
			switch {
			case v.Value.Integer != nil:
				switch v.Name {
				case feedbackReady:
					fb.ready = int32(*v.Value.Integer)
				case feedbackUpdated:
					fb.updated = int32(*v.Value.Integer)
				case feedbackAvailable:
					fb.available = int32(*v.Value.Integer)
				}
			case v.Value.String != nil:
				switch v.Name {
				case feedbackAdmission:
					fb.admission = *v.Value.String
				case feedbackHeartbeat:
					if t, err := time.Parse(time.RFC3339, *v.Value.String); err == nil {
						fb.heartbeat = t
					}
				}
			}
		}
	}
	return fb
}

// setCond records a condition, stamped with the reconciler's clock (not the wall
// clock) so a bound measured from a transition, like the switchover timeout, uses
// the same time as everything else the controller decides.
func setCond(cs *api.CattleSet, at time.Time, condType string, ok bool, reason, message string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             nonEmpty(reason, "Unknown"),
		Message:            message,
		ObservedGeneration: cs.Generation,
		LastTransitionTime: metav1.NewTime(at),
	})
}

func setUnknownCond(cs *api.CattleSet, at time.Time, condType, reason, message string) {
	meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cs.Generation,
		LastTransitionTime: metav1.NewTime(at),
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

// postureFlipped reports whether the primary DC moved between two known holders
// (a realized failover). A first observation (empty prior) or a loss of the
// Lease (empty holder) is not counted as a flip.
func postureFlipped(prior, holder string) bool {
	return prior != "" && holder != "" && prior != holder
}
