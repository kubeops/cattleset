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

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	"k8s.io/apimachinery/pkg/util/intstr"
)

func rules(specs ...[2]string) []petsetapi.DistributionRule {
	out := make([]petsetapi.DistributionRule, 0, len(specs))
	for _, s := range specs {
		out = append(out, petsetapi.DistributionRule{ClusterName: s[0], Role: petsetapi.DCRole(s[1])})
	}
	return out
}

func replicasOf(m map[string]SiteDesired) map[string]int32 {
	out := map[string]int32{}
	for k, v := range m {
		out[k] = v.Replicas
	}
	return out
}

func TestComputePostureActivePassive(t *testing.T) {
	in := PostureInput{
		Mode:            api.CattleModeActivePassive,
		Replicas:        6,
		StandbyReplicas: 6,
		Rules:           rules([2]string{"dc-a", "Member"}, [2]string{"dc-b", "Member"}, [2]string{"dc-c", "Arbiter"}),
		Holder:          "dc-a",
	}
	got := ComputePosture(in)

	if got["dc-a"].Posture != api.CattlePostureActive || got["dc-a"].Replicas != 6 {
		t.Errorf("holder dc-a = %+v, want Active/6", got["dc-a"])
	}
	if got["dc-b"].Posture != api.CattlePostureStandby || got["dc-b"].Replicas != 6 {
		t.Errorf("standby dc-b = %+v, want Standby/6", got["dc-b"])
	}
	if got["dc-c"].Posture != api.CattlePostureNone || got["dc-c"].Replicas != 0 {
		t.Errorf("arbiter dc-c = %+v, want None/0", got["dc-c"])
	}
}

func TestComputePostureHolderFlip(t *testing.T) {
	in := PostureInput{
		Mode:            api.CattleModeActivePassive,
		Replicas:        4,
		StandbyReplicas: 2,
		Rules:           rules([2]string{"dc-a", "Member"}, [2]string{"dc-b", "Member"}),
		Holder:          "dc-b",
	}
	got := ComputePosture(in)
	if got["dc-b"].Replicas != 4 || got["dc-b"].Posture != api.CattlePostureActive {
		t.Errorf("new holder dc-b = %+v, want Active/4", got["dc-b"])
	}
	if got["dc-a"].Replicas != 2 || got["dc-a"].Posture != api.CattlePostureStandby {
		t.Errorf("demoted dc-a = %+v, want Standby/2", got["dc-a"])
	}
}

func TestComputePostureFailClosedUnknownHolder(t *testing.T) {
	in := PostureInput{
		Mode:            api.CattleModeActivePassive,
		Replicas:        5,
		StandbyReplicas: 5,
		Rules:           rules([2]string{"dc-a", "Member"}, [2]string{"dc-b", "Member"}),
		Holder:          "", // unreadable Lease: fail closed
	}
	got := ComputePosture(in)
	for _, c := range []string{"dc-a", "dc-b"} {
		if got[c].Posture != api.CattlePostureStandby {
			t.Errorf("%s posture = %v, want Standby (fail closed)", c, got[c].Posture)
		}
	}
}

func TestComputePostureActiveActiveEvenSplit(t *testing.T) {
	in := PostureInput{
		Mode:     api.CattleModeActiveActive,
		Replicas: 6,
		Rules:    rules([2]string{"dc-a", "Member"}, [2]string{"dc-b", "Member"}, [2]string{"dc-c", "Arbiter"}),
	}
	got := replicasOf(ComputePosture(in))
	if got["dc-a"] != 3 || got["dc-b"] != 3 {
		t.Errorf("even split = %v, want dc-a=3 dc-b=3", got)
	}
	if got["dc-c"] != 0 {
		t.Errorf("arbiter dc-c = %d, want 0", got["dc-c"])
	}
}

func TestComputePostureActiveActiveLargestRemainder(t *testing.T) {
	in := PostureInput{
		Mode:     api.CattleModeActiveActive,
		Replicas: 5,
		Rules:    rules([2]string{"dc-a", "Member"}, [2]string{"dc-b", "Member"}),
	}
	got := replicasOf(ComputePosture(in))
	if total := got["dc-a"] + got["dc-b"]; total != 5 {
		t.Errorf("total = %d, want exactly 5", total)
	}
	// Deterministic: the earlier index gets the remainder.
	if got["dc-a"] != 3 || got["dc-b"] != 2 {
		t.Errorf("split = %v, want dc-a=3 dc-b=2", got)
	}
}

func TestComputePostureActiveActiveWeighted(t *testing.T) {
	in := PostureInput{
		Mode:     api.CattleModeActiveActive,
		Replicas: 9,
		Distribution: []api.CattleShare{
			{ClusterName: "dc-a", Weight: 2},
			{ClusterName: "dc-b", Weight: 1},
		},
		Rules: rules([2]string{"dc-a", "Member"}, [2]string{"dc-b", "Member"}),
	}
	got := replicasOf(ComputePosture(in))
	if got["dc-a"] != 6 || got["dc-b"] != 3 {
		t.Errorf("weighted split = %v, want dc-a=6 dc-b=3", got)
	}
}

func TestComputePostureActiveActiveSurge(t *testing.T) {
	in := PostureInput{
		Mode:     api.CattleModeActiveActive,
		Replicas: 6,
		Surge:    intstr.FromString("100%"),
		Rules:    rules([2]string{"dc-a", "Member"}, [2]string{"dc-b", "Member"}),
		Stale:    map[string]bool{"dc-b": true},
	}
	got := replicasOf(ComputePosture(in))
	// dc-b base share is 3 and it is lost; dc-a absorbs 100% of it.
	if got["dc-a"] != 6 {
		t.Errorf("survivor dc-a = %d, want 6 (3 base + 3 surge)", got["dc-a"])
	}
	// dc-b keeps its base share desired so it returns in-share when OCM reapplies.
	if got["dc-b"] != 3 {
		t.Errorf("lost dc-b = %d, want 3 (base share left desired)", got["dc-b"])
	}
}

func TestComputePostureWitnessIsArbiter(t *testing.T) {
	in := PostureInput{
		Mode:            api.CattleModeActivePassive,
		Replicas:        3,
		StandbyReplicas: 3,
		Rules:           rules([2]string{"dc-a", "Member"}, [2]string{"dc-b", "Member"}, [2]string{"dc-w", "Witness"}),
		Holder:          "dc-a",
	}
	got := ComputePosture(in)
	if got["dc-w"].Role != petsetapi.DCRoleArbiter {
		t.Errorf("witness role = %v, want collapsed to Arbiter", got["dc-w"].Role)
	}
	if got["dc-w"].Replicas != 0 || got["dc-w"].Posture != api.CattlePostureNone {
		t.Errorf("witness dc-w = %+v, want None/0 (a herd holds no data)", got["dc-w"])
	}
}
