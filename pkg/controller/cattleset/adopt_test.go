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
)

// TestAdoptionNoPodTemplateDiff proves the flip restarts no pods: the local
// non-distributed Deployment and the adopted per-site Deployment render the
// identical pod template, so their revision hashes match and a server-side apply
// produces no diff.
func TestAdoptionNoPodTemplateDiff(t *testing.T) {
	cs := distributedHerd()

	local := renderDeployment(cs) // the C3 non-distributed output
	site, err := renderSiteDeployment(cs, SiteDesired{ClusterName: "dc-a", Role: petsetapi.DCRoleMember, Replicas: 6})
	if err != nil {
		t.Fatal(err)
	}

	if got, want := revisionHash(site.Spec.Template), revisionHash(local.Spec.Template); got != want {
		t.Errorf("adopted site revision %q != local revision %q: the flip would restart pods", got, want)
	}
}

func members2() map[string]SiteDesired {
	return map[string]SiteDesired{
		"dc-a": {ClusterName: "dc-a", Role: petsetapi.DCRoleMember, Posture: api.CattlePostureActive, Replicas: 6},
		"dc-b": {ClusterName: "dc-b", Role: petsetapi.DCRoleMember, Posture: api.CattlePostureStandby, Replicas: 6},
	}
}

func TestApplyAdoptionKeepsIncumbentActiveWhileEnabling(t *testing.T) {
	desired := members2()
	// Pretend the holder moved so ComputePosture demoted dc-a to standby.
	demoted := desired["dc-a"]
	demoted.Posture = api.CattlePostureStandby
	demoted.Replicas = 2
	desired["dc-a"] = demoted

	applyAdoption(desired, "dc-a", 6, true)

	if desired["dc-a"].Posture != api.CattlePostureActive || desired["dc-a"].Replicas != 6 {
		t.Errorf("incumbent dc-a should stay Active/6 during enablement: %+v", desired["dc-a"])
	}
}

func TestApplyAdoptionReleasesIncumbentWhenReady(t *testing.T) {
	desired := members2()
	demoted := desired["dc-a"]
	demoted.Posture = api.CattlePostureStandby
	demoted.Replicas = 2
	desired["dc-a"] = demoted

	applyAdoption(desired, "dc-a", 6, false) // enabling done: all sites ready

	if desired["dc-a"].Posture != api.CattlePostureStandby || desired["dc-a"].Replicas != 2 {
		t.Errorf("once ready the incumbent must drop to its declared posture: %+v", desired["dc-a"])
	}
}

func TestApplyAdoptionIgnoresNonMemberIncumbent(t *testing.T) {
	desired := map[string]SiteDesired{
		"dc-c": {ClusterName: "dc-c", Role: petsetapi.DCRoleArbiter, Posture: api.CattlePostureNone},
	}
	applyAdoption(desired, "dc-c", 6, true)
	if desired["dc-c"].Posture != api.CattlePostureNone || desired["dc-c"].Replicas != 0 {
		t.Errorf("a non-Member incumbent must not be activated: %+v", desired["dc-c"])
	}
}
