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

// siteRevision is the revision a site should carry: the pod template, plus that
// site's patch. A change that only touches a sitePatch changes the revision of
// that site alone, so it is sequenced like any other rollout instead of landing
// everywhere at once. A site without a patch keeps the plain template hash.
func siteRevision(cs *api.CattleSet, cluster string) string {
	patch := sitePatchFor(cs, cluster)
	if len(patch) == 0 {
		return revisionHash(cs.Spec.Template)
	}
	raw, err := json.Marshal(cs.Spec.Template)
	if err != nil {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write(raw)
	_, _ = h.Write(patch)
	return fmt.Sprintf("%x", h.Sum32())
}

// rolloutSite is one site's rollout view: its current revision and rollout
// feedback, used to decide whether the next site may move.
type rolloutSite struct {
	ClusterName     string
	Posture         api.CattlePosture
	Admitted        bool
	TargetRevision  string
	CurrentRevision string
	Updated         int32
	Available       int32
	Desired         int32
}

// siteConverged reports whether a site has fully taken the target revision: its
// Deployment reports the new revision, every desired replica is updated, and
// availability is within the strategy's maxUnavailable. Because the admission
// gate does not touch pod readiness, a standby's rollout converges normally.
func siteConverged(s rolloutSite, maxUnavailable int32) bool {
	if s.CurrentRevision != s.TargetRevision {
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
func rolloutPlan(order api.CattleRolloutOrder, maxUnavailable int32, sites []rolloutSite, hold bool) map[string]bool {
	allowed := map[string]bool{}
	if hold {
		return allowed // paused, or a switchover or failover is settling: hold every site's template
	}

	if order == api.CattleRolloutParallel {
		for _, s := range sites {
			allowed[s.ClusterName] = true
		}
		return allowed
	}

	for _, s := range orderSites(order, sites) {
		if siteConverged(s, maxUnavailable) {
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

// orderSites sequences the sites for a rollout. The site that is serving traffic
// (admitted) is always last: that is a fact observed on the spoke, not inferred
// from a Lease, so an unknown or lagging holder cannot send a bad template to the
// serving site first. StandbyFirst then breaks ties by posture, and every order
// finishes by cluster name for determinism.
func orderSites(order api.CattleRolloutOrder, sites []rolloutSite) []rolloutSite {
	out := make([]rolloutSite, len(sites))
	copy(out, sites)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Admitted != out[j].Admitted {
			return !out[i].Admitted
		}
		if order == api.CattleRolloutStandbyFirst {
			if ip, jp := postureRank(out[i].Posture), postureRank(out[j].Posture); ip != jp {
				return ip < jp
			}
		}
		return out[i].ClusterName < out[j].ClusterName
	})
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
