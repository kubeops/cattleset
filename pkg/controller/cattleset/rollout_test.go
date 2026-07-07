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

	core "k8s.io/api/core/v1"
)

const (
	oldRev = "old"
	newRev = "new"
)

func TestRevisionHashChangesWithTemplate(t *testing.T) {
	a := core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{Name: "api", Image: "v1"}}}}
	b := core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{Name: "api", Image: "v2"}}}}
	if revisionHash(a) == revisionHash(b) {
		t.Error("a template change must change the revision hash")
	}
	first := revisionHash(a)
	second := revisionHash(*a.DeepCopy())
	if first != second {
		t.Error("the revision hash must be stable across equal templates")
	}
}

// TestStandbyFirstContainsBadImage proves the active site never moves to the new
// (bad) revision until the standby has converged: the core StandbyFirst
// containment property.
func TestStandbyFirstContainsBadImage(t *testing.T) {
	sites := []rolloutSite{
		{ClusterName: "dc-a", Posture: api.CattlePostureActive, CurrentRevision: oldRev, Updated: 6, Available: 6, Desired: 6},
		{ClusterName: "dc-b", Posture: api.CattlePostureStandby, CurrentRevision: oldRev, Updated: 6, Available: 6, Desired: 6},
	}
	// Round 1: standby (dc-b) is allowed, the active (dc-a) is held.
	plan := rolloutPlan(api.CattleRolloutStandbyFirst, newRev, 1, sites, false)
	if !plan["dc-b"] {
		t.Error("standby dc-b should be allowed to roll first")
	}
	if plan["dc-a"] {
		t.Error("active dc-a must not roll before the standby converges (bad image containment)")
	}
}

func TestStandbyFirstReleasesActiveAfterStandbyConverges(t *testing.T) {
	sites := []rolloutSite{
		{ClusterName: "dc-a", Posture: api.CattlePostureActive, CurrentRevision: oldRev, Updated: 6, Available: 6, Desired: 6},
		{ClusterName: "dc-b", Posture: api.CattlePostureStandby, CurrentRevision: newRev, Updated: 6, Available: 6, Desired: 6},
	}
	plan := rolloutPlan(api.CattleRolloutStandbyFirst, newRev, 1, sites, false)
	if !plan["dc-a"] {
		t.Error("active dc-a should be released once the standby has converged to the new revision")
	}
}

// TestWedgedStandbyStopsSequence proves a standby that never becomes available
// (a bad image) stops the sequence, so the active site is never reached.
func TestWedgedStandbyStopsSequence(t *testing.T) {
	sites := []rolloutSite{
		{ClusterName: "dc-a", Posture: api.CattlePostureActive, CurrentRevision: oldRev, Updated: 6, Available: 6, Desired: 6},
		{ClusterName: "dc-b", Posture: api.CattlePostureStandby, CurrentRevision: newRev, Updated: 2, Available: 0, Desired: 6},
	}
	plan := rolloutPlan(api.CattleRolloutStandbyFirst, newRev, 1, sites, false)
	if plan["dc-a"] {
		t.Error("a wedged standby must stop the sequence, holding the active site")
	}
}

func TestOneSiteAtATime(t *testing.T) {
	sites := []rolloutSite{
		{ClusterName: "dc-a", CurrentRevision: oldRev, Updated: 3, Available: 3, Desired: 3},
		{ClusterName: "dc-b", CurrentRevision: oldRev, Updated: 3, Available: 3, Desired: 3},
		{ClusterName: "dc-c", CurrentRevision: oldRev, Updated: 3, Available: 3, Desired: 3},
	}
	plan := rolloutPlan(api.CattleRolloutOneSiteAtATime, newRev, 1, sites, false)
	// Only the first site (by name) rolls.
	if !plan["dc-a"] || plan["dc-b"] || plan["dc-c"] {
		t.Errorf("OneSiteAtATime should roll only dc-a first: %v", plan)
	}
}

func TestParallelAllowsAll(t *testing.T) {
	sites := []rolloutSite{
		{ClusterName: "dc-a", CurrentRevision: oldRev, Desired: 3},
		{ClusterName: "dc-b", CurrentRevision: oldRev, Desired: 3},
	}
	plan := rolloutPlan(api.CattleRolloutParallel, newRev, 1, sites, false)
	if !plan["dc-a"] || !plan["dc-b"] {
		t.Errorf("Parallel should allow all sites: %v", plan)
	}
}

func TestPausedBlocksTemplatePropagation(t *testing.T) {
	sites := []rolloutSite{
		{ClusterName: "dc-a", Posture: api.CattlePostureStandby, CurrentRevision: oldRev, Desired: 3},
	}
	plan := rolloutPlan(api.CattleRolloutStandbyFirst, newRev, 1, sites, true)
	if len(plan) != 0 {
		t.Errorf("paused must block all template propagation: %v", plan)
	}
}
