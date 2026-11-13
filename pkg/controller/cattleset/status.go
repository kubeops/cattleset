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
	"sort"
	"strings"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	apps "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// projectLocalStatus mirrors an owned Deployment's status aggregates into the
// CattleSet status, and publishes status.selector for the scale subresource so
// an HPA attached to the CattleSet matches the herd's pods. This is the
// non-distributed projection (one Deployment, one set of aggregates).
func projectLocalStatus(cs *api.CattleSet, dep *apps.Deployment) {
	cs.Status.ObservedGeneration = cs.Generation
	cs.Status.Selector = metav1.FormatLabelSelector(cs.Spec.Selector)

	st := dep.Status
	cs.Status.Replicas = st.Replicas
	cs.Status.UpdatedReplicas = st.UpdatedReplicas
	cs.Status.ReadyReplicas = st.ReadyReplicas
	cs.Status.AvailableReplicas = st.AvailableReplicas
	cs.Status.UnavailableReplicas = st.UnavailableReplicas
	cs.Status.CollisionCount = st.CollisionCount

	setConditionFromDeployment(cs, dep, apps.DeploymentAvailable, api.ConditionAvailable)
	setConditionFromDeployment(cs, dep, apps.DeploymentProgressing, api.ConditionProgressing)
}

// setConditionFromDeployment copies the named Deployment condition onto the
// CattleSet under the CattleSet condition name.
func setConditionFromDeployment(cs *api.CattleSet, dep *apps.Deployment, depType apps.DeploymentConditionType, csType string) {
	var found *apps.DeploymentCondition
	for j := range dep.Status.Conditions {
		if dep.Status.Conditions[j].Type == depType {
			found = &dep.Status.Conditions[j]
		}
	}
	if found == nil {
		return
	}
	meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{
		Type:               csType,
		Status:             metav1.ConditionStatus(found.Status),
		Reason:             nonEmpty(found.Reason, "Reconciled"),
		Message:            found.Message,
		ObservedGeneration: cs.Generation,
	})
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// statusFacts is everything writeStatus needs, gathered by the reconcile.
type statusFacts struct {
	pp      *petsetapi.PlacementPolicy
	rules   []petsetapi.DistributionRule
	ord     Ordering
	derived Derived
	appObs  LeaseObservation
	health  map[string]LeaseObservation
	views   map[string]siteView
	sites   []SiteDesired
	hold    bool
	swPlan  SwitchoverPlan
	missing map[syncRef]bool
	depsErr error
	now     time.Time

	stalled      bool
	stalledKnown bool
}

// writeStatus projects the reconcile's result onto cs.Status. It only sets
// fields; the caller writes it.
func (r *Reconciler) writeStatus(cs *api.CattleSet, f statusFacts) {
	priorHolder := ""
	if cs.Status.FailoverGroup != nil {
		priorHolder = cs.Status.FailoverGroup.LeaseHolder
	}
	holder := priorHolder
	if f.appObs.HolderKnown() {
		holder = f.appObs.Holder
	}

	siteStatuses := make([]api.CattleSiteStatus, 0, len(f.sites))
	var totalReplicas, totalReady, totalUpdated, totalAvailable int32
	var serving []string
	converged := true
	gateAgentStale := false

	for _, site := range f.sites {
		ss := api.CattleSiteStatus{
			ClusterName: site.ClusterName,
			Role:        site.Role,
			Posture:     site.Posture,
			Desired:     site.Replicas,
		}
		if site.Role == petsetapi.DCRoleMember {
			v := f.views[site.ClusterName]
			ss.Ready = v.Feedback.ready
			ss.Updated = v.Feedback.updated
			ss.Revision = v.Feedback.revision
			ss.Healthy = v.Healthy
			ss.Admitted = v.Admitted
			ss.AdmissionReason = v.AdmissionReason
			if !v.Feedback.heartbeat.IsZero() {
				t := metav1.NewTime(v.Feedback.heartbeat)
				ss.ObservedAt = &t
			}
			if v.Admitted {
				serving = append(serving, site.ClusterName)
			}
			totalReplicas += site.Replicas
			totalReady += v.Feedback.ready
			totalUpdated += v.Feedback.updated
			totalAvailable += v.Feedback.available
			if v.Feedback.ready < site.Replicas {
				converged = false
			}
			if f.derived.Gate != nil && v.Healthy && v.HeartbeatStale {
				gateAgentStale = true
			}
			siteReady.WithLabelValues(cs.Namespace, cs.Name, site.ClusterName).Set(float64(v.Feedback.ready))
			siteDesired.WithLabelValues(cs.Namespace, cs.Name, site.ClusterName).Set(float64(site.Replicas))
		}
		siteStatuses = append(siteStatuses, ss)
	}
	sort.Strings(serving)

	// A holder change with a known prior is a realized posture flip: record the
	// event and the metric, and stamp the failover time (the spec is unchanged,
	// so generation does not move).
	if postureFlipped(priorHolder, holder) {
		postureFlipsTotal.WithLabelValues(cs.Namespace, cs.Name).Inc()
		now := metav1.NewTime(f.now)
		cs.Status.LastFailoverTime = &now
		r.event(cs, "Normal", "PostureFlip", "primary DC "+priorHolder+" -> "+holder)
	}
	if converged && cs.Status.LastFailoverTime != nil {
		observeToConvergeSeconds.WithLabelValues(cs.Namespace, cs.Name).
			Observe(f.now.Sub(cs.Status.LastFailoverTime.Time).Seconds())
	}

	cs.Status.ObservedGeneration = cs.Generation
	cs.Status.Selector = metav1.FormatLabelSelector(cs.Spec.Selector)
	cs.Status.ServingDCs = serving
	cs.Status.DerivedMode = f.derived.Mode
	cs.Status.Sites = siteStatuses
	cs.Status.Replicas = totalReplicas
	cs.Status.ReadyReplicas = totalReady
	cs.Status.UpdatedReplicas = totalUpdated
	cs.Status.AvailableReplicas = totalAvailable
	cs.Status.UnavailableReplicas = 0
	if totalReplicas >= totalAvailable {
		cs.Status.UnavailableReplicas = totalReplicas - totalAvailable
	}

	cs.Status.FailoverGroup = nil
	if f.ord.GroupName != "" {
		fg := &api.CattleFailoverGroupStatus{Name: f.ord.GroupName, LeaseHolder: holder}
		if g := f.ord.Group; g != nil {
			fg.Phase, fg.ActiveDC = string(g.Status.Phase), g.Status.ActiveDC
		}
		cs.Status.FailoverGroup = fg
	}
	cs.Status.Dependencies = nil
	activeOf := map[string]string{}
	for _, dg := range f.ord.DepGroups {
		activeOf[dg.Name] = dg.ActiveDC
	}
	for _, d := range f.ord.Deps {
		cs.Status.Dependencies = append(cs.Status.Dependencies, api.CattleDependencyStatus{
			Group: d.Group, APIGroup: d.Member.APIGroup, Kind: d.Member.Kind,
			Namespace: d.Member.Namespace, Name: d.Member.Name,
			Camp: string(d.Camp), ActiveDC: activeOf[d.Group],
		})
	}

	setCond(cs, f.now, api.ConditionPostureConverged, converged, ifElse(converged, "PostureConverged", "SitesConverging"), "")
	setCond(cs, f.now, api.ConditionAvailable, totalAvailable > 0, ifElse(totalAvailable > 0, "MinimumReplicasAvailable", "NoAvailableReplicas"), "")
	setCond(cs, f.now, api.ConditionProgressing, !converged, ifElse(converged, "Converged", "Progressing"), "")

	groupPhase := ""
	if f.ord.Group != nil {
		groupPhase = string(f.ord.Group.Status.Phase)
	}
	groupMoving := groupPhase == string(petsetapi.FailoverGroupPhaseWaitingForMembers) || groupPhase == string(petsetapi.FailoverGroupPhaseWaitingForDependency)
	setCond(cs, f.now, api.ConditionFailoverInProgress, f.swPlan.Active() || groupMoving, ifElse(f.swPlan.Active(), "Switchover", ifElse(groupMoving, groupPhase, "Steady")), "")
	switch {
	case f.ord.GroupName == "":
		setCond(cs, f.now, api.ConditionLeaseAccessible, true, "NoFailoverGroup", "the herd follows no Lease")
	case f.appObs.Err != nil:
		setCond(cs, f.now, api.ConditionLeaseAccessible, false, "ReadError", f.appObs.Err.Error())
	default:
		setCond(cs, f.now, api.ConditionLeaseAccessible, f.appObs.Found, ifElse(f.appObs.Found, "Readable", "NotFound"), "")
	}

	setCond(cs, f.now, api.ConditionAdmitted, len(serving) > 0, ifElse(len(serving) > 0, "Admitted", "NotAdmitted"), "")
	setCond(cs, f.now, api.ConditionOrderingConfigured, f.derived.OK, f.derived.Reason, f.derived.Message)

	depsReady, depsMsg := dependenciesReady(f.ord)
	setCond(cs, f.now, api.ConditionDependenciesReady, depsReady, ifElse(depsReady, "Ready", "NotReady"), depsMsg)

	blocked := f.ord.Group != nil && f.ord.Group.Status.Phase == petsetapi.FailoverGroupPhaseBlocked
	blockedMsg := ""
	if blocked {
		for _, c := range f.ord.Group.Status.Conditions {
			if c.Type == "Ready" {
				blockedMsg = c.Message
			}
		}
	}
	setCond(cs, f.now, api.ConditionGroupBlocked, blocked, ifElse(blocked, "Blocked", "NotBlocked"), blockedMsg)

	if f.stalledKnown {
		setCond(cs, f.now, api.ConditionDataTierStalled, f.stalled, ifElse(f.stalled, "PromotionStalled", "NotStalled"), "")
	} else {
		setUnknownCond(cs, f.now, api.ConditionDataTierStalled, "Unknown", "a data tier member could not be read")
	}

	setCond(cs, f.now, api.ConditionSwitchoverInProgress, f.swPlan.Active(), ifElse(f.swPlan.Step == StepNone, "NoSwitchover", string(f.swPlan.Step)), f.swPlan.Message)
	setCond(cs, f.now, api.ConditionCapacityHeld, f.hold, ifElse(f.hold, "PrimaryNotEstablished", "PrimaryEstablished"), ifElse(f.hold, "the Lease holder cannot be established; capacity is held at its last value", ""))
	setCond(cs, f.now, api.ConditionGateAgentStale, gateAgentStale, ifElse(gateAgentStale, "HeartbeatStale", "Heartbeating"), "")

	switch refs := syncRefs(cs); {
	case len(refs) == 0:
		setCond(cs, f.now, api.ConditionDependenciesSynced, true, "NothingToSync", "")
	case f.depsErr != nil:
		setUnknownCond(cs, f.now, api.ConditionDependenciesSynced, "ReadError", f.depsErr.Error())
	case len(f.missing) > 0:
		names := make([]string, 0, len(f.missing))
		for m := range f.missing {
			names = append(names, m.String())
		}
		sort.Strings(names)
		setCond(cs, f.now, api.ConditionDependenciesSynced, false, "SourceMissing", "not found on the hub: "+strings.Join(names, ", "))
	default:
		applied := true
		for _, site := range f.sites {
			if site.Role == petsetapi.DCRoleMember && !f.views[site.ClusterName].DepsApplied {
				applied = false
			}
		}
		setCond(cs, f.now, api.ConditionDependenciesSynced, applied, ifElse(applied, "Synced", "NotApplied"), "")
	}
}

// dependenciesReady is true when every direct dependency group is active on the
// same DC: the DC the herd's group then targets.
func dependenciesReady(ord Ordering) (bool, string) {
	if len(ord.DepGroups) == 0 {
		return true, "no dependencies"
	}
	target := ""
	for _, dg := range ord.DepGroups {
		switch {
		case !dg.Found:
			return false, "dependency " + dg.Name + " does not exist"
		case dg.ActiveDC == "":
			return false, "dependency " + dg.Name + " is not active on any data center yet"
		case target != "" && dg.ActiveDC != target:
			return false, "dependencies are active on different data centers"
		}
		target = dg.ActiveDC
	}
	return true, "dependencies are active on " + target
}

func ifElse(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}
