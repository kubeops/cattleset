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
	"time"

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
	// RevisionLabel records the pod-template revision a rendered per-site
	// Deployment carries, so cross-site rollout skew is visible.
	RevisionLabel = GroupName + "/revision"
	// ManifestWorkClusterNameLabel is the OCM managed-cluster-name label the
	// work agent keys on; it records which spoke a ManifestWork targets.
	ManifestWorkClusterNameLabel = "open-cluster-management.io/cluster-name"

	// FieldManager is the single server-side-apply field manager the
	// controller writes every rendered object with (the single-writer rule).
	FieldManager = "cattleset-controller"

	// GateFieldManager is the field manager the spoke gate agent owns the
	// Service selector with after creation (design Section 6).
	GateFieldManager = "cattleset-gate-agent"

	// GateAnnotation carries, on a gated Service, the JSON-encoded gate spec the
	// hub renders for the spoke gate agent (see pkg/controller/gate). The agent
	// has no hub client: everything it needs is in this annotation and in local
	// objects.
	GateAnnotation = GroupName + "/gate"

	// AdmissionAnnotation is written by the gate agent on the Service: "Admitted"
	// or a reason code. AdmissionHeartbeatAnnotation is the RFC3339 time of its
	// last evaluation. Both are scalars so ManifestWork JSONPath feedback can
	// project them back to the hub.
	AdmissionAnnotation          = GroupName + "/admission"
	AdmissionHeartbeatAnnotation = GroupName + "/admission-observed-at"

	// ServingLabel is the pod label the gate agent sets to true or false so an
	// application can close long lived sessions through the downward API. It must
	// not reuse AdmittedSelectorKey: that key is the unmatched Service selector
	// term, and a pod carrying it would be selected by an unadmitted Service.
	ServingLabel = GroupName + "/serving"

	// CleanupFinalizer removes a CattleSet's ManifestWorks on delete.
	CleanupFinalizer = GroupName + "/cattleset-cleanup"

	// FailoverGroupFieldManager is the field manager this controller reports its
	// FailoverGroup member entry with.
	FailoverGroupFieldManager = "cattleset-failovergroup"

	// AnnSwitchoverTo and AnnSwitchoverAbort are the planned switchover contract
	// shared with every DC-DR engine. The strings are dr-controlplane's and the
	// database operators', copied, with a test that pins them.
	AnnSwitchoverTo    = "dr.kubedb.com/switchover-to"
	AnnSwitchoverAbort = "dr.kubedb.com/switchover-abort"

	// CoordNamespace and the Lease and marker names are dr-controlplane's
	// (pkg/leases): the same convention, copied, with a test that pins it.
	CoordNamespace     = "dc-failover"
	GlobalPrimaryLease = "primary-dc"
	GroupPrimaryPrefix = "primary-dc-"
	HealthLeasePrefix  = "dc-health-"
	OverrideSuffix     = "-override"

	// Marker data keys (dr-controlplane agent). The marker ConfigMap is named like
	// the Lease and lives in CoordNamespace on every spoke.
	MarkerKeyActiveDC = "activeDC"
	MarkerKeyRenew    = "renewTime"
	MarkerKeyQuiesce  = "quiesce"

	// DBRoleLabel and DBRolePrimaryValue are what the database sets on a writable
	// primary pod.
	DBRoleLabel        = "kubedb.com/role"
	DBRolePrimaryValue = "primary"

	// AdoptClusterAnnotation names the Member cluster whose existing local
	// Deployment is adopted when distributed flips false to true. During
	// enablement that site stays active regardless of mode until every site is
	// ready, so there is no traffic gap. When unset, adoption falls back to the
	// Lease holder, then the first Member.
	AdoptClusterAnnotation = GroupName + "/adopt-cluster"
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

// Mode returns the declared posture mode: the empty string in non-distributed
// mode (where no topology is present), and Auto for a distributed herd that
// leaves it unset.
func (cs *CattleSet) Mode() CattleMode {
	if cs.Spec.Topology == nil {
		if cs.Spec.Distributed {
			return CattleModeAuto
		}
		return ""
	}
	if cs.Spec.Topology.Mode == "" {
		return CattleModeAuto
	}
	return cs.Spec.Topology.Mode
}

// Defaults for CattleAdmission, the values the design fixes.
const (
	DefaultMinReadyReplicas int32 = 1
	// DefaultMarkerMaxAge is pg-coordinator's marker TTL; a larger value is
	// rejected.
	DefaultMarkerMaxAge = 30 * time.Second
	// DefaultPrimaryLossGrace rides out an intra-DC primary re-election (4 to 6 s
	// for Postgres) without flapping admission.
	DefaultPrimaryLossGrace = 15 * time.Second
)

// MinReady returns the replicas a site needs before it is reported ready.
func (a *CattleAdmission) MinReady() int32 {
	if a == nil || a.MinReadyReplicas == nil {
		return DefaultMinReadyReplicas
	}
	return *a.MinReadyReplicas
}

// MarkerMaxAgeOrDefault returns the configured marker age or the default.
func (a *CattleAdmission) MarkerMaxAgeOrDefault() time.Duration {
	if a == nil || a.MarkerMaxAge == nil {
		return DefaultMarkerMaxAge
	}
	return a.MarkerMaxAge.Duration
}

// PrimaryLossGraceOrDefault returns the configured grace or the default. An
// explicit zero disables the grace.
func (a *CattleAdmission) PrimaryLossGraceOrDefault() time.Duration {
	if a == nil || a.PrimaryLossGrace == nil {
		return DefaultPrimaryLossGrace
	}
	return a.PrimaryLossGrace.Duration
}

// DrainPodLabel reports whether the gate agent labels pods serving=true|false.
func (a *CattleAdmission) DrainPodLabel() bool {
	if a == nil || a.Drain == nil || a.Drain.PodLabel == nil {
		return true
	}
	return *a.Drain.PodLabel
}

// HasTraffic reports whether the herd renders a Service.
func (cs *CattleSet) HasTraffic() bool {
	return cs.Spec.Traffic != nil && cs.Spec.Traffic.ServiceName != ""
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
