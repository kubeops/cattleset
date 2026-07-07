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

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	"k8s.io/apimachinery/pkg/util/intstr"
)

// SiteDesired is the computed desired state for one cluster in a distributed
// herd: the effective role (Witness collapsed into Arbiter), the posture, and
// the replica count the rendered Deployment carries there.
type SiteDesired struct {
	ClusterName string
	Role        petsetapi.DCRole
	Posture     api.CattlePosture
	Replicas    int32
}

// PostureInput is everything ComputePosture needs, gathered from the CattleSet
// spec, the referenced PlacementPolicy, and the Lease follower. Passing it as a
// value keeps ComputePosture a pure function.
type PostureInput struct {
	Mode            api.CattleMode
	Replicas        int32
	StandbyReplicas int32
	Distribution    []api.CattleShare
	Surge           intstr.IntOrString
	Rules           []petsetapi.DistributionRule

	// Holder is the Lease holderIdentity (the primary DC). Empty means unknown
	// or unreadable: fail closed, so no site is treated as primary.
	Holder string

	// Stale marks clusters whose per-DC health Lease is stale (a lost Member in
	// ActiveActive). Its share is absorbed by survivors via Surge.
	Stale map[string]bool
}

// effectiveRole collapses Witness into Arbiter: a herd holds no data, so a
// Witness rule is a non-serving vote exactly like an Arbiter.
func effectiveRole(r petsetapi.DCRole) petsetapi.DCRole {
	if r == "" {
		return petsetapi.DCRoleMember
	}
	if r == petsetapi.DCRoleWitness {
		return petsetapi.DCRoleArbiter
	}
	return r
}

// ComputePosture derives each site's desired replica count from the mode, the
// DC roles, the Lease holder, and per-DC health. Pods run only in Member DCs;
// every other DC gets zero. It is a pure function, unit-testable without a
// cluster.
func ComputePosture(in PostureInput) map[string]SiteDesired {
	out := make(map[string]SiteDesired, len(in.Rules))

	var members []string
	for _, r := range in.Rules {
		role := effectiveRole(r.Role)
		out[r.ClusterName] = SiteDesired{
			ClusterName: r.ClusterName,
			Role:        role,
			Posture:     api.CattlePostureNone,
			Replicas:    0,
		}
		if role == petsetapi.DCRoleMember {
			members = append(members, r.ClusterName)
		}
	}

	switch in.Mode {
	case api.CattleModeActivePassive:
		for _, c := range members {
			sd := out[c]
			if in.Holder != "" && c == in.Holder {
				sd.Posture = api.CattlePostureActive
				sd.Replicas = in.Replicas
			} else {
				// Fail closed: a non-holder (or, when the holder is unknown,
				// every Member) is standby.
				sd.Posture = api.CattlePostureStandby
				sd.Replicas = in.StandbyReplicas
			}
			out[c] = sd
		}

	case api.CattleModeActiveActive:
		computeActiveActive(in, members, out)
	}

	return out
}

func computeActiveActive(in PostureInput, members []string, out map[string]SiteDesired) {
	if len(members) == 0 {
		return
	}
	// Base weighted split of Replicas across all Members (even by default).
	weights := make([]int32, len(members))
	for i, c := range members {
		weights[i] = weightFor(c, in.Distribution)
	}
	base := largestRemainder(in.Replicas, weights)
	baseByCluster := map[string]int32{}
	for i, c := range members {
		baseByCluster[c] = base[i]
	}

	// Sum the lost share (stale Members) and the surviving Members.
	var lostShare int32
	var survivors []string
	var survivorWeights []int32
	for i, c := range members {
		if in.Stale[c] {
			lostShare += base[i]
		} else {
			survivors = append(survivors, c)
			survivorWeights = append(survivorWeights, weights[i])
		}
	}

	// Distribute the surge (a fraction of the lost share) across survivors.
	surgeTotal := int32(0)
	if lostShare > 0 && len(survivors) > 0 {
		v, err := intstr.GetScaledValueFromIntOrPercent(&in.Surge, int(lostShare), true)
		if err == nil {
			surgeTotal = int32(v)
		}
		if surgeTotal > lostShare {
			surgeTotal = lostShare
		}
	}
	surge := largestRemainder(surgeTotal, survivorWeights)
	surgeByCluster := map[string]int32{}
	for i, c := range survivors {
		surgeByCluster[c] = surge[i]
	}

	for _, c := range members {
		sd := out[c]
		sd.Posture = api.CattlePostureActive
		// A stale Member's ManifestWork is left at its base share (OCM applies
		// it on return); survivors add the surge.
		sd.Replicas = baseByCluster[c] + surgeByCluster[c]
		out[c] = sd
	}
}

// weightFor returns a cluster's ActiveActive weight: the value from the
// distribution when listed, else 1 (an even split).
func weightFor(cluster string, dist []api.CattleShare) int32 {
	for _, s := range dist {
		if s.ClusterName == cluster {
			if s.Weight <= 0 {
				return 1
			}
			return s.Weight
		}
	}
	return 1
}

// largestRemainder splits total across the given weights so the parts sum
// exactly to total, giving the remainder to the largest fractional shares (and,
// on ties, to the earlier index for determinism).
func largestRemainder(total int32, weights []int32) []int32 {
	n := len(weights)
	out := make([]int32, n)
	if n == 0 || total <= 0 {
		return out
	}
	var sumW int64
	for _, w := range weights {
		sumW += int64(w)
	}
	if sumW == 0 {
		return out
	}

	type frac struct {
		idx int
		rem int64 // fractional remainder numerator (over sumW)
	}
	var assigned int32
	fracs := make([]frac, n)
	for i, w := range weights {
		exact := int64(total) * int64(w)
		q := exact / sumW
		out[i] = int32(q)
		assigned += int32(q)
		fracs[i] = frac{idx: i, rem: exact % sumW}
	}

	remaining := total - assigned
	sort.SliceStable(fracs, func(a, b int) bool {
		if fracs[a].rem != fracs[b].rem {
			return fracs[a].rem > fracs[b].rem
		}
		return fracs[a].idx < fracs[b].idx
	})
	for i := int32(0); i < remaining; i++ {
		out[fracs[i].idx]++
	}
	return out
}
