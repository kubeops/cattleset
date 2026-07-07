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

package v1

import (
	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// AdmittedSelectorKey is the controller-owned admission term added to a
	// gated herd's Service selector. No pod carries this label, so an
	// unadmitted Service selects no endpoints. The spoke gate agent admits
	// a site by removing this term and revokes by restoring it.
	AdmittedSelectorKey   = GroupName + "/admitted"
	AdmittedSelectorValue = "true"

	// CattleSetNameLabel identifies the CattleSet that owns a rendered
	// object or ManifestWork. Ownership must not be resolved by selector
	// alone (two herds of one application can have overlapping selectors).
	CattleSetNameLabel      = GroupName + "/cattleset-name"
	CattleSetNamespaceLabel = GroupName + "/cattleset-namespace"
	// ClusterNameLabel records the Member cluster a rendered object targets.
	ClusterNameLabel = GroupName + "/cluster-name"
	// ManifestWorkClusterNameLabel is the OCM managed-cluster-name label the
	// work agent keys on; it records which spoke a ManifestWork targets.
	ManifestWorkClusterNameLabel = "open-cluster-management.io/cluster-name"

	// FieldManager is the single server-side-apply field manager the
	// controller writes every rendered object with (the single-writer rule).
	FieldManager = "cattleset-controller"
)

// FromDeployment converts a Deployment into a non-distributed CattleSet by
// copying the spec core verbatim (a literal field copy, the spec core being
// DeploymentSpec) and carrying labels and annotations. No extension field is
// set: the result is a Deployment in all but kind.
func FromDeployment(d *apps.Deployment) *CattleSet {
	cs := &CattleSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: SchemeGroupVersion.String(),
			Kind:       ResourceKindCattleSet,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        d.Name,
			Namespace:   d.Namespace,
			Labels:      copyStringMap(d.Labels),
			Annotations: copyStringMap(d.Annotations),
		},
		Spec: CattleSetSpec{
			Replicas:                d.Spec.Replicas,
			Selector:                d.Spec.Selector.DeepCopy(),
			Template:                *d.Spec.Template.DeepCopy(),
			Strategy:                *d.Spec.Strategy.DeepCopy(),
			MinReadySeconds:         d.Spec.MinReadySeconds,
			RevisionHistoryLimit:    d.Spec.RevisionHistoryLimit,
			Paused:                  d.Spec.Paused,
			ProgressDeadlineSeconds: d.Spec.ProgressDeadlineSeconds,
		},
	}
	return cs
}

// DeploymentSpecCore extracts the Deployment core from a CattleSet spec as an
// apps/v1 DeploymentSpec. This is the inverse of the mirror and the literal
// field copy the renderer uses to emit a rendered Deployment.
func (cs *CattleSet) DeploymentSpecCore() apps.DeploymentSpec {
	return apps.DeploymentSpec{
		Replicas:                cs.Spec.Replicas,
		Selector:                cs.Spec.Selector.DeepCopy(),
		Template:                *cs.Spec.Template.DeepCopy(),
		Strategy:                *cs.Spec.Strategy.DeepCopy(),
		MinReadySeconds:         cs.Spec.MinReadySeconds,
		RevisionHistoryLimit:    cs.Spec.RevisionHistoryLimit,
		Paused:                  cs.Spec.Paused,
		ProgressDeadlineSeconds: cs.Spec.ProgressDeadlineSeconds,
	}
}

// IsDistributed reports whether the herd renders per-site Deployments through
// ManifestWorks (true) or manages a single local Deployment (false).
func (cs *CattleSet) IsDistributed() bool {
	return cs.Spec.Distributed
}

// Mode returns the declared posture mode, or the empty string in
// non-distributed mode (where no topology is present).
func (cs *CattleSet) Mode() CattleMode {
	if cs.Spec.Topology == nil {
		return ""
	}
	return cs.Spec.Topology.Mode
}

// IsGated reports whether the herd couples its traffic admission to a data tier.
func (cs *CattleSet) IsGated() bool {
	return cs.Spec.Gates != nil && len(cs.Spec.Gates.DataTiers) > 0
}

// OffsetLabels are the identity labels stamped on every object the controller
// renders for this CattleSet, used to attribute ownership unambiguously.
func (cs *CattleSet) OffsetLabels() map[string]string {
	return map[string]string{
		CattleSetNameLabel:      cs.Name,
		CattleSetNamespaceLabel: cs.Namespace,
	}
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
