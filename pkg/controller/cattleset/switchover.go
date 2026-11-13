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
	"time"
)

// SwitchoverTimeout is how long a planned switchover may run before it is
// abandoned, the same bound the database engines use.
const SwitchoverTimeout = 10 * time.Minute

// SwitchoverStep is what the handler decided this pass.
type SwitchoverStep string

const (
	// StepNone: no switchover requested.
	StepNone SwitchoverStep = "None"
	// StepReject: the request cannot converge; drop it.
	StepReject SwitchoverStep = "Reject"
	// StepAbort: a human asked to abort.
	StepAbort SwitchoverStep = "Abort"
	// StepTimeout: it ran longer than SwitchoverTimeout.
	StepTimeout SwitchoverStep = "Timeout"
	// StepWaitCapacity: the target does not yet have the pods it needs.
	StepWaitCapacity SwitchoverStep = "WaitingForCapacity"
	// StepRequestHandoff: ask dr-controlplane to move the Lease.
	StepRequestHandoff SwitchoverStep = "RequestingHandoff"
	// StepDone: the Lease holder is the target.
	StepDone SwitchoverStep = "Done"
)

// SwitchoverInput is everything the handler decides from.
type SwitchoverInput struct {
	// Target is the dr.kubedb.com/switchover-to value, "" when absent.
	Target string
	// Abort is true when dr.kubedb.com/switchover-abort is present.
	Abort bool
	// Started is when the switchover was first observed (zero: now).
	Started time.Time
	Now     time.Time

	// Members are the Member DCs of the herd's placement policy.
	Members []string
	// HasDeps says the herd's group has dependencies; DepActiveDCs is each direct
	// dependency group's activeDC ("" when it is not active anywhere).
	HasDeps      bool
	DepActiveDCs []string

	AppHolder   string
	HolderKnown bool
	// TargetReady: the target site is healthy and has the replicas it needs.
	TargetReady bool
}

// SwitchoverPlan is the handler's decision. It is a function of annotations and
// status only, so a leader change resumes it.
type SwitchoverPlan struct {
	Step    SwitchoverStep
	Message string
	// HandoffTo is what to report on the member entry; empty withdraws the request.
	HandoffTo string
	// ClearAnnotations removes switchover-to and switchover-abort from the
	// CattleSet. Removal on every exit path is mandatory: the FailoverGroup
	// controller leaves a member that still carries switchover-to alone, so a
	// leftover annotation means the group can never be asked again.
	ClearAnnotations bool
}

// Active reports whether the switchover is still running.
func (p SwitchoverPlan) Active() bool {
	return p.Step == StepWaitCapacity || p.Step == StepRequestHandoff
}

// PlanSwitchover decides one pass of the planned switchover (design 4.4).
//
// There is no data to catch up for a stateless tier, so a planned move costs pod
// readiness plus one Lease handoff. The handoff itself is not done here: the plan
// only sets handoffTo on the herd's member entry, and the FailoverGroup controller
// moves the Lease once every member agrees.
func PlanSwitchover(in SwitchoverInput) SwitchoverPlan {
	if in.Target == "" {
		return SwitchoverPlan{Step: StepNone}
	}
	if in.Abort {
		return SwitchoverPlan{Step: StepAbort, Message: "switchover aborted on request", ClearAnnotations: true}
	}
	if !in.Started.IsZero() && in.Now.Sub(in.Started) > SwitchoverTimeout {
		return SwitchoverPlan{Step: StepTimeout, Message: "switchover timed out", ClearAnnotations: true}
	}

	isMember := false
	for _, m := range in.Members {
		if m == in.Target {
			isMember = true
		}
	}
	if !isMember {
		return SwitchoverPlan{Step: StepReject, Message: in.Target + " is not a Member DC of the placement policy", ClearAnnotations: true}
	}
	// A group that depends on others can only be where they are; asking for any
	// other DC could never converge.
	if in.HasDeps {
		for _, dc := range in.DepActiveDCs {
			if dc != in.Target {
				return SwitchoverPlan{Step: StepReject, Message: "the data tier is not active on " + in.Target + "; a dependent group can only move to where its dependencies are", ClearAnnotations: true}
			}
		}
	}

	if in.HolderKnown && in.AppHolder == in.Target {
		return SwitchoverPlan{Step: StepDone, Message: "the Lease is held by " + in.Target, ClearAnnotations: true}
	}
	if !in.TargetReady {
		return SwitchoverPlan{Step: StepWaitCapacity, Message: "waiting for capacity on " + in.Target}
	}
	return SwitchoverPlan{Step: StepRequestHandoff, Message: "requested the Lease handoff to " + in.Target, HandoffTo: in.Target}
}

// MemberReadyInput is what decides whether a herd reports itself ready to its
// FailoverGroup on a given DC.
type MemberReadyInput struct {
	// Healthy: the DC's health Lease is fresh and the site's ManifestWork is
	// applied and available. A dead DC is never ready, whatever its stale feedback
	// still says (review R3).
	Healthy bool
	// AvailableReplicas is the site's available pods; MinReady is the floor.
	AvailableReplicas int32
	MinReady          int32
	// Admitted is the site's Service selecting endpoints.
	Admitted bool
	// DependencyElsewhere: at least one direct dependency is active on a DC other
	// than this one.
	DependencyElsewhere bool
}

// MemberReady is the ready bit the herd reports for the DC where its own Lease is
// held (design 4.3). It is the conjunction of
//
//	healthy, capacity, and serving
//
// where serving means admitted, or unadmitted only because a dependency is active
// elsewhere. The second clause is the fix for review R1: on the old holder, once
// the database has moved, the application is (correctly) unadmitted, and if that
// counted as not ready the FailoverGroup controller would never ask for the
// switchover (it requires every member ready on the current holder) and the
// application's group would wait forever. On the target the dependency is local,
// so ready still needs real admission and Steady keeps its strict meaning.
func MemberReady(in MemberReadyInput) bool {
	if !in.Healthy || in.AvailableReplicas < in.MinReady {
		return false
	}
	return in.Admitted || in.DependencyElsewhere
}
