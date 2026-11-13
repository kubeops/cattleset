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
	"testing"
	"time"
)

var swNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func swInput(mut func(*SwitchoverInput)) SwitchoverInput {
	in := SwitchoverInput{
		Target: "dc-b", Now: swNow, Members: []string{"dc-a", "dc-b"},
		HasDeps: true, DepActiveDCs: []string{"dc-b"},
		AppHolder: "dc-a", HolderKnown: true, TargetReady: true,
	}
	if mut != nil {
		mut(&in)
	}
	return in
}

func TestPlanSwitchover(t *testing.T) {
	cases := []struct {
		name      string
		mut       func(*SwitchoverInput)
		step      SwitchoverStep
		handoffTo string
		clear     bool
	}{
		{"nothing requested", func(in *SwitchoverInput) { in.Target = "" }, StepNone, "", false},
		{"request the handoff once the target is ready", nil, StepRequestHandoff, "dc-b", false},
		{"wait for capacity first", func(in *SwitchoverInput) { in.TargetReady = false }, StepWaitCapacity, "", false},
		{"done when the Lease is on the target", func(in *SwitchoverInput) { in.AppHolder = "dc-b" }, StepDone, "", true},
		{"an unknown holder is never 'done'", func(in *SwitchoverInput) { in.AppHolder, in.HolderKnown = "dc-b", false }, StepRequestHandoff, "dc-b", false},

		{"abort wins and clears the annotation", func(in *SwitchoverInput) { in.Abort = true }, StepAbort, "", true},
		{"timeout clears the annotation", func(in *SwitchoverInput) { in.Started = swNow.Add(-SwitchoverTimeout - time.Second) }, StepTimeout, "", true},
		{"just inside the timeout keeps going", func(in *SwitchoverInput) { in.Started = swNow.Add(-SwitchoverTimeout + time.Second) }, StepRequestHandoff, "dc-b", false},

		{"reject a target that is not a Member", func(in *SwitchoverInput) { in.Target = "dc-w"; in.DepActiveDCs = []string{"dc-w"} }, StepReject, "", true},
		{"reject a target the data tier is not on", func(in *SwitchoverInput) { in.DepActiveDCs = []string{"dc-a"} }, StepReject, "", true},
		{"reject when any dependency disagrees", func(in *SwitchoverInput) { in.DepActiveDCs = []string{"dc-b", "dc-a"} }, StepReject, "", true},
		{"a dependency not active anywhere cannot be moved to", func(in *SwitchoverInput) { in.DepActiveDCs = []string{""} }, StepReject, "", true},
		{"no dependencies: any Member is fine", func(in *SwitchoverInput) { in.HasDeps, in.DepActiveDCs = false, nil }, StepRequestHandoff, "dc-b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PlanSwitchover(swInput(tc.mut))
			if p.Step != tc.step || p.HandoffTo != tc.handoffTo || p.ClearAnnotations != tc.clear {
				t.Fatalf("got %+v, want step=%s handoffTo=%q clear=%v", p, tc.step, tc.handoffTo, tc.clear)
			}
		})
	}
}

// Every way out of a switchover clears the annotation: otherwise the
// FailoverGroup controller never asks this member again.
func TestEveryExitClearsTheAnnotation(t *testing.T) {
	for _, step := range []SwitchoverStep{StepReject, StepAbort, StepTimeout, StepDone} {
		found := false
		for _, mut := range []func(*SwitchoverInput){
			func(in *SwitchoverInput) { in.Target = "dc-w" },
			func(in *SwitchoverInput) { in.Abort = true },
			func(in *SwitchoverInput) { in.Started = swNow.Add(-time.Hour) },
			func(in *SwitchoverInput) { in.AppHolder = "dc-b" },
		} {
			if p := PlanSwitchover(swInput(mut)); p.Step == step {
				found = true
				if !p.ClearAnnotations || p.Active() || p.HandoffTo != "" {
					t.Fatalf("%s must clear and withdraw the request: %+v", step, p)
				}
			}
		}
		if !found {
			t.Fatalf("no scenario produced %s", step)
		}
	}
}

// TestMemberReadyTraceTable is design 4.3's trace of a planned database
// switchover A to B. Review R1: with the old definition (ready only when
// admitted) the third row was false and the application's group hung there.
func TestMemberReadyTraceTable(t *testing.T) {
	healthy := func(mut func(*MemberReadyInput)) MemberReadyInput {
		in := MemberReadyInput{Healthy: true, AvailableReplicas: 6, MinReady: 1}
		mut(&in)
		return in
	}
	rows := []struct {
		moment string
		in     MemberReadyInput
		want   bool
	}{
		{"steady: data on A, app admitted on A", healthy(func(in *MemberReadyInput) { in.Admitted = true }), true},
		{"data quiescing on A: app unadmitted, data still active on A", healthy(func(in *MemberReadyInput) {}), false},
		{"data active on B: app on A is unadmitted but ready (R1)", healthy(func(in *MemberReadyInput) { in.DependencyElsewhere = true }), true},
		{"app on B (the target): dependency is local, so it needs real admission", healthy(func(in *MemberReadyInput) {}), false},
		{"app admitted on B", healthy(func(in *MemberReadyInput) { in.Admitted = true }), true},
	}
	for _, r := range rows {
		if got := MemberReady(r.in); got != r.want {
			t.Errorf("%s: ready = %v, want %v", r.moment, got, r.want)
		}
	}
}

func TestMemberReadyNeedsAHealthyDCWithCapacity(t *testing.T) {
	ok := MemberReadyInput{Healthy: true, AvailableReplicas: 3, MinReady: 2, Admitted: true}
	if !MemberReady(ok) {
		t.Fatal("setup")
	}
	// Review R3: a dead DC keeps its last ManifestWork feedback, and must not
	// keep reporting ready with it.
	dead := ok
	dead.Healthy = false
	if MemberReady(dead) {
		t.Fatal("an unhealthy DC is never ready, even admitted")
	}
	dead.DependencyElsewhere = true
	if MemberReady(dead) {
		t.Fatal("nor when a dependency is elsewhere")
	}
	short := ok
	short.AvailableReplicas = 1
	if MemberReady(short) {
		t.Fatal("below minReadyReplicas is not ready")
	}
}
