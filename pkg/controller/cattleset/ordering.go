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
	"strings"

	api "kubeops.dev/cattleset/apis/apps/v1"
	"kubeops.dev/cattleset/pkg/controller/gate"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reasons for the OrderingConfigured condition.
const (
	ReasonConfigured           = "Configured"
	ReasonNoFailoverGroup      = "NoFailoverGroup"
	ReasonFailoverGroupMissing = "FailoverGroupMissing"
	ReasonDependencyUnresolved = "DependencyUnresolved"
	ReasonAppSharesDataGroup   = "AppSharesDataGroup"
	ReasonModeConflict         = "ModeConflict"
	ReasonTightened            = "Tightened"
)

// Dependency is one data tier the herd depends on: a member of a FailoverGroup
// named in the herd's group's dependsOn.
type Dependency struct {
	Group  string
	Member petsetapi.FailoverGroupMember
	Camp   gate.Camp
}

// DepGroup is one group named in dependsOn.
type DepGroup struct {
	Name     string
	Found    bool
	ActiveDC string
	Phase    petsetapi.FailoverGroupPhase
	Members  int
}

// Ordering is the herd's FailoverGroup and what it depends on, as read from the
// hub. Unresolved is empty only when everything the gate needs is known.
type Ordering struct {
	// GroupName is the FailoverGroup the placement policy references, "" when it
	// names none.
	GroupName string
	Group     *petsetapi.FailoverGroup
	DepGroups []DepGroup
	Deps      []Dependency

	// Unresolved is one of ReasonFailoverGroupMissing, ReasonDependencyUnresolved
	// or ReasonAppSharesDataGroup, or "" when the ordering is usable.
	Unresolved string
	Message    string
}

// groupNameFor returns the FailoverGroup a placement policy references.
func groupNameFor(pp *petsetapi.PlacementPolicy) string {
	if pp == nil || pp.Spec.ClusterSpreadConstraint == nil || pp.Spec.ClusterSpreadConstraint.FailoverPolicy == nil {
		return ""
	}
	if ref := pp.Spec.ClusterSpreadConstraint.FailoverPolicy.FailoverGroupRef; ref != nil {
		return ref.Name
	}
	return ""
}

// resolveOrdering reads the herd's FailoverGroup, the groups it depends on, and
// the members those report. A group that does not exist, or a dependency with no
// members yet, makes the ordering unresolved, which renders a closed gate: "no
// members" is also exactly what the controller sees before the database operator
// has reported, and that must never read as "nothing to wait for".
func (r *Reconciler) resolveOrdering(ctx context.Context, groupName string) (Ordering, error) {
	ord := Ordering{GroupName: groupName}
	if groupName == "" {
		return ord, nil
	}
	g := &petsetapi.FailoverGroup{}
	if err := r.Get(ctx, client.ObjectKey{Name: groupName}, g); err != nil {
		if apierrors.IsNotFound(err) {
			ord.Unresolved, ord.Message = ReasonFailoverGroupMissing, "FailoverGroup "+groupName+" does not exist"
			return ord, nil
		}
		return ord, err
	}
	ord.Group = g

	// An application must not share a group with a database: it would be a plain
	// member with no ordering behind it.
	for _, m := range g.Status.Members {
		if m.APIGroup != api.GroupName {
			ord.Unresolved = ReasonAppSharesDataGroup
			ord.Message = "FailoverGroup " + groupName + " also holds " + m.Kind + " " + m.Namespace + "/" + m.Name + "; an application needs its own group that dependsOn the data group"
			return ord, nil
		}
	}

	var missing []string
	for _, dep := range g.Spec.DependsOn {
		dg := &petsetapi.FailoverGroup{}
		if err := r.Get(ctx, client.ObjectKey{Name: dep}, dg); err != nil {
			if apierrors.IsNotFound(err) {
				ord.DepGroups = append(ord.DepGroups, DepGroup{Name: dep})
				missing = append(missing, dep+" (missing)")
				continue
			}
			return ord, err
		}
		ord.DepGroups = append(ord.DepGroups, DepGroup{Name: dep, Found: true, ActiveDC: dg.Status.ActiveDC, Phase: dg.Status.Phase, Members: len(dg.Status.Members)})
		if len(dg.Status.Members) == 0 {
			missing = append(missing, dep+" (no members have reported)")
			continue
		}
		for _, m := range dg.Status.Members {
			ord.Deps = append(ord.Deps, Dependency{Group: dep, Member: m, Camp: gate.CampFor(m.APIGroup, m.Kind)})
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		ord.Unresolved = ReasonDependencyUnresolved
		ord.Message = "dependencies not resolved: " + strings.Join(missing, ", ")
	}
	return ord, nil
}

// Derived is the posture and gate the controller renders for a herd.
type Derived struct {
	// Mode is ActiveActive or ActivePassive: the posture the herd really has.
	Mode api.CattleMode
	// Gate is the spec rendered into the Service annotation, or nil for a Service
	// with no admission at all (Open: always admitted, nothing to check).
	Gate *gate.GateSpec
	// Reason and Message feed the OrderingConfigured condition.
	Reason  string
	Message string
	// OK is true when the ordering is configured and the gate is what the user
	// asked for. A false value always comes with a closed or restrictive gate.
	OK bool
}

// deriveGate decides the posture and the gate (design 6.1). prior is the mode a
// previous reconcile derived, and specChanged says the CattleSet spec changed
// since: the derived mode tightens by itself and never loosens by itself, so a
// dependency whose members vanish (or a FailoverGroup edited) cannot open a gate;
// relaxing needs a change to the CattleSet.
func deriveGate(cs *api.CattleSet, ord Ordering, prior api.CattleMode, specChanged bool) Derived {
	explicit := cs.Mode()
	adm := cs.Spec.Admission
	base := gate.GateSpec{
		Version:                 gate.SpecVersion,
		MarkerMaxAgeSeconds:     int32(adm.MarkerMaxAgeOrDefault().Seconds()),
		PrimaryLossGraceSeconds: int32(adm.PrimaryLossGraceOrDefault().Seconds()),
		DrainPodLabel:           adm.DrainPodLabel(),
	}
	closed := func(mode api.CattleMode, reason, msg string) Derived {
		g := base
		g.Mode = gate.GateModeClosed
		return Derived{Mode: mode, Gate: &g, Reason: reason, Message: msg}
	}
	// The mode to report while closed: what the user asked for, with Auto read as
	// the restrictive one.
	closedMode := explicit
	if closedMode == api.CattleModeAuto {
		closedMode = api.CattleModeActivePassive
	}

	switch {
	case ord.GroupName == "":
		if explicit == api.CattleModeActiveActive {
			return Derived{Mode: api.CattleModeActiveActive, Reason: ReasonConfigured, Message: "ActiveActive with no data dependency", OK: true}
		}
		return closed(api.CattleModeActivePassive, ReasonNoFailoverGroup, "the placement policy names no failoverGroupRef, so there is nothing to order or gate on")
	case ord.Unresolved != "":
		return closed(closedMode, ord.Unresolved, ord.Message)
	}

	// The ordering is resolved from here on.
	groupLease := primaryLeaseName(ord.GroupName)
	tiers := make([]gate.GateTier, 0, len(ord.Deps))
	allLeaderless := len(ord.Deps) > 0
	for _, d := range ord.Deps {
		if d.Camp != gate.CampLeaderlessGeoNative {
			allLeaderless = false
		}
		tiers = append(tiers, gate.GateTier{
			Lease:            primaryLeaseName(d.Group),
			Namespace:        d.Member.Namespace,
			Name:             d.Member.Name,
			Kind:             d.Member.Kind,
			Camp:             d.Camp,
			Service:          d.Member.Name,
			InstanceSelector: map[string]string{"app.kubernetes.io/instance": d.Member.Name},
		})
	}

	var out Derived
	switch {
	case len(ord.Deps) == 0 && len(ord.Group.Spec.DependsOn) == 0:
		if explicit == api.CattleModeActivePassive {
			g := base
			g.Mode, g.AppLease = gate.GateModeAppMarker, groupLease
			out = Derived{Mode: api.CattleModeActivePassive, Gate: &g, Reason: ReasonConfigured, OK: true}
		} else {
			out = Derived{Mode: api.CattleModeActiveActive, Reason: ReasonConfigured, OK: true}
		}
	default:
		g := base
		g.Mode, g.Tiers = gate.GateModeTiers, tiers
		out = Derived{Gate: &g, Reason: ReasonConfigured, OK: true}
		switch {
		case explicit == api.CattleModeActivePassive:
			out.Mode = api.CattleModeActivePassive
		case allLeaderless && explicit != api.CattleModeActivePassive:
			out.Mode = api.CattleModeActiveActive
		default:
			out.Mode = api.CattleModeActivePassive
			if explicit == api.CattleModeActiveActive {
				// Rejected by the webhook when it can see the dependencies; here the
				// controller tightens and says so.
				out.Reason, out.OK = ReasonModeConflict, false
				out.Message = "ActiveActive is not supported over a " + string(tiers[0].Camp) + " data tier: the application reaches its database only through the local Service, which has no endpoints in a standby DC"
			}
		}
	}

	// Tighten only. Auto may not relax from ActivePassive without a spec change.
	if explicit == api.CattleModeAuto && prior == api.CattleModeActivePassive && !specChanged && out.Mode == api.CattleModeActiveActive {
		out.Mode = api.CattleModeActivePassive
		out.Reason = ReasonTightened
		out.Message = "the derived mode stays ActivePassive until the CattleSet spec changes"
		if out.Gate == nil {
			g := base
			g.Mode, g.AppLease = gate.GateModeAppMarker, groupLease
			out.Gate = &g
		}
	}
	return out
}

// activeSet decides which Member DCs carry the full replica count in
// ActivePassive, and whether capacity must hold because nothing could be
// established (design 6.3).
//
//   - Scale up early: a Member is active as soon as it holds the herd's Lease, or
//     holds the Lease of any dependency group (that Lease moves about 40 s before
//     the database is writable, which a warm or cold standby uses to start pods),
//     or is admitted.
//   - Scale down late: a Member that was active stays active until the herd's
//     group is Steady on another DC.
//   - Hold on unknown: if no Lease holder could be established at all, nothing
//     changes. The caller then keeps the previous counts.
func activeSet(members []string, appObs LeaseObservation, depObs []LeaseObservation, admitted map[string]bool, prev map[string]SiteDesired, group *petsetapi.FailoverGroup) (active map[string]bool, hold bool) {
	active = map[string]bool{}
	known := false
	if appObs.HolderKnown() {
		active[appObs.Holder] = true
		known = true
	}
	for _, o := range depObs {
		if o.HolderKnown() {
			active[o.Holder] = true
			known = true
		}
	}
	for dc, ok := range admitted {
		if ok {
			active[dc] = true
		}
	}
	steadyElsewhere := func(dc string) bool {
		return group != nil && group.Status.Phase == petsetapi.FailoverGroupPhaseSteady && group.Status.ActiveDC != "" && group.Status.ActiveDC != dc
	}
	for _, c := range members {
		if p, ok := prev[c]; ok && p.Posture == api.CattlePostureActive && !active[c] && !steadyElsewhere(c) {
			active[c] = true
		}
	}
	return active, !known
}
