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
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	kmapi "kmodules.xyz/client-go/api/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	ResourceKindCattleSet = "CattleSet"
	ResourceCattleSet     = "cattleset"
	ResourceCattleSets    = "cattlesets"
)

// +genclient
// +genclient:method=GetScale,verb=get,subresource=scale,result=k8s.io/api/autoscaling/v1.Scale
// +genclient:method=UpdateScale,verb=update,subresource=scale,input=k8s.io/api/autoscaling/v1.Scale,result=k8s.io/api/autoscaling/v1.Scale
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".spec.topology.mode"
// +kubebuilder:printcolumn:name="Primary",type="string",JSONPath=".status.currentPrimaryDC"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Sites",type="string",JSONPath=".status.sites[*].clusterName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CattleSet runs a stateless application across data centers as one unit.
// It renders one Deployment per Member data center and follows the
// primary-DC Lease selected by the referenced PlacementPolicy.
type CattleSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec CattleSetSpec `json:"spec,omitempty"`
	// +optional
	Status CattleSetStatus `json:"status,omitempty"`
}

type CattleSetSpec struct {
	// ---------------------------------------------------------------
	// Deployment core. These fields mirror apps/v1 DeploymentSpec
	// field for field (names, types, json tags, and defaults, per
	// k8s.io/api apps/v1/types.go), so a Deployment spec pastes into
	// a CattleSet unchanged.
	// ---------------------------------------------------------------

	// Number of desired pods, in the ACTIVE posture: for ActivePassive,
	// the count in the Lease holder DC; for ActiveActive, the total
	// across Member DCs; for non-distributed, the Deployment's count.
	// Defaults to 1.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Label selector for pods. Must match the pod template's labels.
	Selector *metav1.LabelSelector `json:"selector"`

	// Template describes the pods that will be created. Identical in
	// every site by construction; per site deltas go in sitePatches,
	// never into rendered objects. The only allowed
	// template.spec.restartPolicy value is "Always".
	Template core.PodTemplateSpec `json:"template"`

	// The deployment strategy to use to replace existing pods with new
	// ones, applied per site.
	// +optional
	// +patchStrategy=retainKeys
	Strategy apps.DeploymentStrategy `json:"strategy,omitempty" patchStrategy:"retainKeys"`

	// Minimum number of seconds for which a newly created pod should be
	// ready without any of its containers crashing, for it to be
	// considered available. Defaults to 0.
	// +optional
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`

	// The number of old ReplicaSets to retain to allow rollback, per
	// rendered Deployment. Defaults to 10.
	// +optional
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`

	// Indicates that the CattleSet is paused: template rollouts stop,
	// but posture changes still apply (a failover is never paused).
	// +optional
	Paused bool `json:"paused,omitempty"`

	// The maximum time in seconds for a rollout to make progress before
	// it is considered to be failed, per site. Defaults to 600.
	// +optional
	ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds,omitempty"`

	// ---------------------------------------------------------------
	// DC/DR extension. All fields below are optional; when every one
	// of them is absent, a CattleSet is a Deployment.
	// ---------------------------------------------------------------

	// Distributed means ManifestWorks manage per cluster Deployments,
	// as in PetSet. False (the default) is non-distributed mode: the
	// CattleSet manages one local Deployment and behaves exactly like it.
	// Flipping false to true is the supported day-2 path to DC/DR.
	// +optional
	Distributed bool `json:"distributed,omitempty"`

	// PodPlacementPolicy references the PlacementPolicy whose
	// clusterSpreadConstraint names the clusters, their DC roles
	// (Member or Arbiter; a Witness rule is treated as Arbiter, since a
	// herd holds no data), the KubeSlice, and the failoverPolicy
	// trigger (which primary-DC Lease this herd follows). In
	// non-distributed mode it may still be referenced for its local
	// constraints (zone and node spread, affinity); a
	// clusterSpreadConstraint then requires Distributed.
	// +optional
	PodPlacementPolicy *core.LocalObjectReference `json:"podPlacementPolicy,omitempty"`

	// Topology declares the posture semantics. Required when Distributed,
	// must be absent otherwise.
	// +optional
	Topology *CattleTopology `json:"topology,omitempty"`

	// SitePatches are strategic merge patches applied to the rendered
	// Deployment for a named cluster (local registry host, site env vars).
	// They cannot touch replicas, selector, or the admission gate.
	// +optional
	SitePatches []SitePatch `json:"sitePatches,omitempty"`

	// RolloutOrder sequences the sites during a template change:
	// StandbyFirst (default for ActivePassive), OneSiteAtATime
	// (default for ActiveActive), or Parallel.
	// +optional
	RolloutOrder CattleRolloutOrder `json:"rolloutOrder,omitempty"`

	// Traffic wires the per site Service, its slice export, the per
	// site HTTPRoute, and GSLB. In non-distributed mode only ServiceName
	// and HTTPRoute are allowed.
	// +optional
	Traffic *CattleTraffic `json:"traffic,omitempty"`

	// Gates couples the herd's traffic admission to its data tier.
	// +optional
	Gates *CattleGates `json:"gates,omitempty"`
}

type CattleMode string

const (
	// CattleModeActiveActive serves from every Member DC at once.
	CattleModeActiveActive CattleMode = "ActiveActive"
	// CattleModeActivePassive serves only from the primary-DC Lease holder.
	CattleModeActivePassive CattleMode = "ActivePassive"
)

type CattleTopology struct {
	// +kubebuilder:validation:Enum=ActiveActive;ActivePassive
	Mode CattleMode `json:"mode"`

	// Distribution optionally weights the ActiveActive split of Replicas
	// across Member clusters. Default: even split. Ignored for ActivePassive.
	// +optional
	Distribution []CattleShare `json:"distribution,omitempty"`

	// SurgeOnFailover is the extra capacity surviving Members add when a
	// Member DC is lost in ActiveActive, as an absolute count or a percent
	// of the lost site's share. Default "100%" (absorb it fully).
	// This is a capacity action, not a correctness one.
	// +optional
	SurgeOnFailover *intstr.IntOrString `json:"surgeOnFailover,omitempty"`

	// StandbyReplicas is the per standby Member replica count in
	// ActivePassive. Default: Replicas (hot standby, RTO is traffic
	// cutover only). 0 is a cold standby (RTO includes pod start).
	// +optional
	StandbyReplicas *int32 `json:"standbyReplicas,omitempty"`
}

type CattleShare struct {
	ClusterName string `json:"clusterName"`
	Weight      int32  `json:"weight"`
}

type SitePatch struct {
	ClusterName string `json:"clusterName"`
	// Patch is a strategic merge patch over the rendered Deployment.
	Patch runtime.RawExtension `json:"patch"`
}

// +kubebuilder:validation:Enum=StandbyFirst;OneSiteAtATime;Parallel
type CattleRolloutOrder string

const (
	CattleRolloutStandbyFirst   CattleRolloutOrder = "StandbyFirst"
	CattleRolloutOneSiteAtATime CattleRolloutOrder = "OneSiteAtATime"
	CattleRolloutParallel       CattleRolloutOrder = "Parallel"
)

type CattleTraffic struct {
	// ServiceName is the per site Service (same name in every cluster)
	// fronting the herd. The controller renders it alongside the Deployment.
	ServiceName string `json:"serviceName"`

	// Export exports the Service on the PlacementPolicy's KubeSlice
	// (ServiceExport), so other slice members reach the herd by one name.
	// +optional
	Export bool `json:"export,omitempty"`

	// GSLB, when set, has the controller maintain a k8gb Gslb per site:
	// strategy failover with primaryGeoTag following the Lease holder
	// (ActivePassive), or roundRobin across healthy Members (ActiveActive).
	// +optional
	GSLB *CattleGSLB `json:"gslb,omitempty"`

	// HTTPRoute, when set, has the controller render one HTTPRoute per
	// site alongside the Service (Gateway API only; legacy Ingress is
	// not rendered), attached to that site's Gateway and backed by the
	// local Service. Routes are steering, never the fence.
	// +optional
	HTTPRoute *CattleHTTPRoute `json:"httpRoute,omitempty"`
}

type CattleGSLB struct {
	Host string `json:"host"`
	// +optional
	TTLSeconds *int32 `json:"ttlSeconds,omitempty"`
}

type CattleHTTPRoute struct {
	// Hostnames defaults to the GSLB host when unset.
	// +optional
	Hostnames []string `json:"hostnames,omitempty"`

	// ParentRefs attach the route to the per site Gateway listeners
	// (the same Gateway name in every cluster, like the Service).
	ParentRefs []gwv1.ParentReference `json:"parentRefs"`
}

type CattleGates struct {
	// DataTiers reference the KubeDB Database objects (or any object
	// exposing status.disasterRecovery) this herd writes to. A site's
	// Service is admitted (selects endpoints) only while every referenced
	// data tier reports that site as its promoted, writable primary.
	// Requires topology.mode ActivePassive. The webhook requires each
	// referenced data tier to follow the SAME failoverPolicy trigger as
	// this herd, so they move together.
	DataTiers []kmapi.TypedObjectReference `json:"dataTiers"`
}

type CattleSetStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CurrentPrimaryDC mirrors the followed Lease's holderIdentity
	// (empty for ActiveActive without a trigger).
	// +optional
	CurrentPrimaryDC string `json:"currentPrimaryDC,omitempty"`

	// Selector is the serialized label selector, published for the
	// scale subresource so HPA can match the herd's pods.
	// +optional
	Selector string `json:"selector,omitempty"`

	// Aggregates across sites (scale subresource reads Replicas).
	// These mirror the apps/v1 DeploymentStatus aggregate fields, so
	// tooling that reads a Deployment's status reads a CattleSet's.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`
	// +optional
	UnavailableReplicas int32 `json:"unavailableReplicas,omitempty"`
	// +optional
	CollisionCount *int32 `json:"collisionCount,omitempty"`

	// +optional
	Sites []CattleSiteStatus `json:"sites,omitempty"`

	// Conditions: Available, Progressing, PostureConverged,
	// FailoverInProgress, WritePathGated, LeaseAccessible.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// +optional
	LastFailoverTime *metav1.Time `json:"lastFailoverTime,omitempty"`
}

// Condition types surfaced on CattleSetStatus.
const (
	ConditionAvailable          = "Available"
	ConditionProgressing        = "Progressing"
	ConditionPostureConverged   = "PostureConverged"
	ConditionFailoverInProgress = "FailoverInProgress"
	ConditionWritePathGated     = "WritePathGated"
	ConditionLeaseAccessible    = "LeaseAccessible"
)

type CattlePosture string

const (
	CattlePostureActive  CattlePosture = "Active"
	CattlePostureStandby CattlePosture = "Standby"
	// CattlePostureNone is any non-Member DC (Witness collapses into
	// Arbiter for a herd).
	CattlePostureNone CattlePosture = "None"
)

type CattleSiteStatus struct {
	ClusterName string           `json:"clusterName"`
	Role        petsetapi.DCRole `json:"role"`
	Posture     CattlePosture    `json:"posture"`
	Desired     int32            `json:"desired"`
	Ready       int32            `json:"ready"`
	Updated     int32            `json:"updated"`
	// +optional
	Revision string `json:"revision,omitempty"`
	// Gated is true while the write-path gate holds this site's
	// Service unadmitted (data tier not promoted here).
	// +optional
	Gated bool `json:"gated,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CattleSetList is a collection of CattleSets.
type CattleSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CattleSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CattleSet{}, &CattleSetList{})
}
