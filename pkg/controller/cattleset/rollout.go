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
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"

	api "kubeops.dev/cattleset/apis/apps/v1"

	core "k8s.io/api/core/v1"
)

// revisionHash is the pod-template revision: a stable short hash of the pod
// template spec. A template change changes the hash, which drives the per-site
// rollout sequencing.
func revisionHash(tmpl core.PodTemplateSpec) string {
	raw, err := json.Marshal(tmpl)
	if err != nil {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write(raw)
	return fmt.Sprintf("%x", h.Sum32())
}

// rolloutSite is one site's rollout view: its current revision and rollout
// feedback, used to decide whether the next site may move.
type rolloutSite struct {
	ClusterName     string
	Posture         api.CattlePosture
	CurrentRevision string
	Updated         int32
	Available       int32
	Desired         int32
}

// siteConverged reports whether a site has fully taken the target revision: its
// Deployment reports the new revision, every desired replica is updated, and
// availability is within the strategy's maxUnavailable. Because the admission
// gate does not touch pod readiness, a standby's rollout converges normally.
func siteConverged(s rolloutSite, target string, maxUnavailable int32) bool {
	if s.CurrentRevision != target {
		return false
	}
	if s.Updated < s.Desired {
		return false
	}
	return s.Available >= s.Desired-maxUnavailable
}

// rolloutPlan decides which sites may move to the target revision this pass,
// honoring the rolloutOrder. paused blocks all template propagation (a failover,
// which is a posture change, is never paused and is handled outside this
// function). A wedged site stops the sequence: the next site waits until the
// previous one converges, so StandbyFirst never touches the active site before
// the standby is healthy, and a bad image is contained.
func rolloutPlan(order api.CattleRolloutOrder, target string, maxUnavailable int32, sites []rolloutSite, paused bool) map[string]bool {
	allowed := map[string]bool{}
	if paused {
		return allowed // hold every site at its current template
	}

	if order == api.CattleRolloutParallel {
		for _, s := range sites {
			allowed[s.ClusterName] = true
		}
		return allowed
	}

	for _, s := range orderSites(order, sites) {
		if siteConverged(s, target, maxUnavailable) {
			allowed[s.ClusterName] = true
			continue
		}
		// The first non-converged site is allowed to move; every site after it
		// waits until this one converges.
		allowed[s.ClusterName] = true
		break
	}
	return allowed
}

// orderSites sequences the sites for a rollout. StandbyFirst updates standby
// sites before the active one; OneSiteAtATime and the default order by cluster
// name for determinism.
func orderSites(order api.CattleRolloutOrder, sites []rolloutSite) []rolloutSite {
	out := make([]rolloutSite, len(sites))
	copy(out, sites)

	byName := func(i, j int) bool { return out[i].ClusterName < out[j].ClusterName }

	if order == api.CattleRolloutStandbyFirst {
		sort.SliceStable(out, func(i, j int) bool {
			// Standby before Active; within a posture, by name.
			ip, jp := postureRank(out[i].Posture), postureRank(out[j].Posture)
			if ip != jp {
				return ip < jp
			}
			return byName(i, j)
		})
		return out
	}

	sort.SliceStable(out, byName)
	return out
}

// postureRank orders standby before active so StandbyFirst updates standbys
// first.
func postureRank(p api.CattlePosture) int {
	if p == api.CattlePostureActive {
		return 1
	}
	return 0
}
