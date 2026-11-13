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
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".status.derivedMode"
// +kubebuilder:printcolumn:name="Serving",type="string",JSONPath=".status.servingDCs"
// +kubebuilder:printcolumn:name="Lease",type="string",JSONPath=".status.failoverGroup.leaseHolder"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Sites",type="string",JSONPath=".status.sites[*].clusterName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CattleSet runs a stateless application across data centers as one unit.
// It renders one Deployment per Member data center, takes part in the
// FailoverGroup ordering named by the referenced PlacementPolicy, and admits
// traffic to a site only while the data tiers it depends on are active there.
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
	// clusterSpreadConstraint names the clusters and their DC roles
	// (Member or Arbiter; a Witness rule is treated as Arbiter, since a
	// herd holds no data), and whose failoverPolicy.failoverGroupRef names
	// the FailoverGroup this herd is a member of. The primary-dc-<group> Lease
	// is derived from that name alone. The herd's FailoverGroup should
	// dependsOn the FailoverGroups of the data tiers it writes to, and must use
	// the same Member and Arbiter DCs as them. The KubeSlice in the policy is
	// not used: a CattleSet never joins a slice. In non-distributed mode the
	// policy may still be referenced for its local constraints (zone and node
	// spread, affinity); a clusterSpreadConstraint then requires Distributed.
	// +optional
	PodPlacementPolicy *core.LocalObjectReference `json:"podPlacementPolicy,omitempty"`

	// Topology declares the posture semantics. Optional when Distributed
	// (mode then defaults to Auto), must be absent otherwise.
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

	// Traffic wires the per site Service, the per site HTTPRoute, and GSLB.
	// In non-distributed mode only ServiceName, Ports and HTTPRoute are
	// allowed.
	// +optional
	Traffic *CattleTraffic `json:"traffic,omitempty"`

	// Admission tunes the spoke-local, fail-closed traffic admission. The set
	// of data tiers it checks is not configured here: it is resolved from the
	// herd's FailoverGroup dependencies.
	// +optional
	Admission *CattleAdmission `json:"admission,omitempty"`

	// SyncResources lists objects in this CattleSet's namespace on the hub that
	// the pods need on every Member cluster. The controller copies them into a
	// per-site ManifestWork (<herd>-deps), which also creates the namespace.
	// Allowed kinds: ConfigMap, Secret, ServiceAccount (image pull secrets are
	// Secrets). Updates are copied; pods are not restarted.
	// +optional
	SyncResources []core.TypedLocalObjectReference `json:"syncResources,omitempty"`

	// Gates is REMOVED. Which data tiers a herd depends on is declared by the
	// FailoverGroup its placement policy references (dependsOn). Any CattleSet
	// that sets this field is rejected by the webhook with that pointer; the
	// field exists only so the rejection can name it, since a structural schema
	// would otherwise drop it silently.
	// +optional
	Gates *CattleGates `json:"gates,omitempty"`
}

type CattleMode string

const (
	// CattleModeAuto derives ActiveActive or ActivePassive from the camps of the
	// data tiers the herd depends on. It is the default.
	CattleModeAuto CattleMode = "Auto"
	// CattleModeActiveActive serves from every Member DC at once.
	CattleModeActiveActive CattleMode = "ActiveActive"
	// CattleModeActivePassive serves only from the DC where the data tiers are
	// active (the herd's own FailoverGroup holder).
	CattleModeActivePassive CattleMode = "ActivePassive"
)

type CattleTopology struct {
	// Mode. Auto (the default) derives the posture and status.derivedMode
	// reports the result. An explicit ActivePassive is always allowed. An
	// explicit ActiveActive over a single-writer or async-mirror dependency is
	// rejected: the application reaches its database only through the local
	// Service, which has no endpoints in a standby DC.
	// +kubebuilder:validation:Enum=Auto;ActiveActive;ActivePassive
	// +kubebuilder:default=Auto
	// +optional
	Mode CattleMode `json:"mode,omitempty"`

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

	// Ports are the Service ports. Required: the controller does not assume a
	// port mapping. HTTPRoute backends use the first port.
	// +kubebuilder:validation:MinItems=1
	Ports []core.ServicePort `json:"ports"`

	// Export is REMOVED: a stateless CattleSet does not use KubeSlice. Setting it
	// is rejected by the webhook.
	// +optional
	Export bool `json:"export,omitempty"`

	// GSLB, when set, has the controller maintain a k8gb Gslb per site:
	// strategy failover with primaryGeoTag following the admitted site
	// (ActivePassive), or roundRobin across healthy Members (ActiveActive).
	// +optional
	GSLB *CattleGSLB `json:"gslb,omitempty"`

	// HTTPRoute, when set, has the controller render one HTTPRoute per
	// site alongside the Service (Gateway API only; legacy Ingress is
	// not rendered), attached to that site's Gateway and backed by the
	// local Service. Routes are steering, never the fence.
	// +optional
	HTTPRoute *CattleHTTPRoute `json:"httpRoute,omitempty"`

	// Network reserves the steering substrate on top of endpoint admission.
	// Placeholder: only None is accepted until the networking decision is made.
	// +optional
	Network *CattleNetwork `json:"network,omitempty"`

	// Signal, when true, publishes the per site admission state on the status
	// so an external load balancer can consume it. Placeholder pending the
	// networking decision.
	// +optional
	Signal bool `json:"signal,omitempty"`
}

// CattleNetwork is reserved. Do not implement until the owner decides.
type CattleNetwork struct {
	// Type: None (default, endpoint admission only), Istio, Cilium. KubeSlice is
	// intentionally not a value. Only None is accepted today.
	// +kubebuilder:validation:Enum=None;Istio;Cilium
	// +optional
	Type string `json:"type,omitempty"`
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

// CattleGates is the removed gates field. See CattleSetSpec.Gates.
type CattleGates struct {
	// +optional
	DataTiers []kmapi.TypedObjectReference `json:"dataTiers,omitempty"`
}

// CattleAdmission tunes how a site decides to receive traffic. Failure policy is
// not configurable: unknown always means unadmitted.
type CattleAdmission struct {
	// MinReadyReplicas is how many available pods a site needs before it is
	// reported ready to the FailoverGroup. Defaults to 1.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinReadyReplicas *int32 `json:"minReadyReplicas,omitempty"`

	// MarkerMaxAge is how old a scope marker may be before it counts as stale.
	// Defaults to 30s, the value pg-coordinator uses. It must not exceed it: the
	// webhook rejects a larger value, because admitting an application in a DC
	// the database already considers inactive defeats the ordering.
	// +optional
	MarkerMaxAge *metav1.Duration `json:"markerMaxAge,omitempty"`

	// PrimaryLossGrace is how long a site stays admitted when the only failing
	// check is "a writable primary exists here" while the marker still names this
	// DC. It rides out an intra-DC primary re-election (4 to 6 s for Postgres).
	// Marker loss and quiesce are never graced. Defaults to 15s; 0 disables.
	// +optional
	PrimaryLossGrace *metav1.Duration `json:"primaryLossGrace,omitempty"`

	// Drain controls session draining when a site is revoked.
	// +optional
	Drain *CattleDrain `json:"drain,omitempty"`
}

type CattleDrain struct {
	// PodLabel, when true (the default), makes the gate agent set
	// apps.k8s.appscode.com/serving=true|false on the herd's pods in the site, so
	// an application can close long-lived sessions through the downward API.
	// +optional
	PodLabel *bool `json:"podLabel,omitempty"`
}

type CattleSetStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ServingDCs are the Member DCs whose Service is admitted right now. This is
	// where traffic is, and can briefly differ from the group's Lease holder.
	// +listType=set
	// +optional
	ServingDCs []string `json:"servingDCs,omitempty"`

	// DerivedMode is the posture Auto resolved to, or the explicit mode.
	// +optional
	DerivedMode CattleMode `json:"derivedMode,omitempty"`

	// FailoverGroup mirrors the herd's own group.
	// +optional
	FailoverGroup *CattleFailoverGroupStatus `json:"failoverGroup,omitempty"`

	// Dependencies lists the data tiers resolved from the group's dependsOn, with
	// the camp used for the gate.
	// +optional
	Dependencies []CattleDependencyStatus `json:"dependencies,omitempty"`

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

	// Conditions: Available, Progressing, PostureConverged, FailoverInProgress,
	// LeaseAccessible, Admitted, OrderingConfigured, DependenciesReady,
	// GroupBlocked, DataTierStalled, SwitchoverInProgress, CapacityHeld,
	// GateAgentStale, DependenciesSynced.
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
	ConditionLeaseAccessible    = "LeaseAccessible"

	// ConditionAdmitted is true while at least one Member site is admitted.
	ConditionAdmitted = "Admitted"
	// ConditionOrderingConfigured is true when the policy names a FailoverGroup
	// that exists and every dependency has reported members.
	ConditionOrderingConfigured = "OrderingConfigured"
	// ConditionDependenciesReady is true when every direct dependency is active
	// on the DC the group targets.
	ConditionDependenciesReady = "DependenciesReady"
	// ConditionGroupBlocked is true when the FailoverGroup phase is Blocked.
	ConditionGroupBlocked = "GroupBlocked"
	// ConditionDataTierStalled is true when a dependency member carries
	// DCDRPromotionStalled.
	ConditionDataTierStalled = "DataTierStalled"
	// ConditionSwitchoverInProgress is true while a switchover-to is handled.
	ConditionSwitchoverInProgress = "SwitchoverInProgress"
	// ConditionCapacityHeld is true while the holder is unknown and capacity is
	// held at its last known value.
	ConditionCapacityHeld = "CapacityHeld"
	// ConditionGateAgentStale is true when a site's admission annotation stopped
	// heartbeating.
	ConditionGateAgentStale = "GateAgentStale"
	// ConditionDependenciesSynced is true when every syncResources entry exists
	// on the hub and is applied on every Member.
	ConditionDependenciesSynced = "DependenciesSynced"
)

type CattlePosture string

const (
	CattlePostureActive  CattlePosture = "Active"
	CattlePostureStandby CattlePosture = "Standby"
	// CattlePostureNone is any non-Member DC (Witness collapses into
	// Arbiter for a herd).
	CattlePostureNone CattlePosture = "None"
)

// CattleFailoverGroupStatus mirrors the herd's own FailoverGroup.
type CattleFailoverGroupStatus struct {
	Name string `json:"name"`
	// Phase is copied from FailoverGroup.status.phase.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	LeaseHolder string `json:"leaseHolder,omitempty"`
	// +optional
	ActiveDC string `json:"activeDC,omitempty"`
}

// CattleDependencyStatus is one data tier resolved from the group's dependsOn.
type CattleDependencyStatus struct {
	Group     string `json:"group"`
	APIGroup  string `json:"apiGroup"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Camp is SingleWriter, AsyncMirror, QuorumNative or LeaderlessGeoNative.
	Camp string `json:"camp"`
	// +optional
	ActiveDC string `json:"activeDC,omitempty"`
}

type CattleSiteStatus struct {
	ClusterName string           `json:"clusterName"`
	Role        petsetapi.DCRole `json:"role"`
	Posture     CattlePosture    `json:"posture"`
	// Desired is the replica count the controller last computed for the site.
	// It is also the value held when the Lease holder cannot be read.
	Desired int32 `json:"desired"`
	Ready   int32 `json:"ready"`
	Updated int32 `json:"updated"`
	// +optional
	Revision string `json:"revision,omitempty"`
	// Admitted is true while the site's Service selects endpoints.
	// +optional
	Admitted bool `json:"admitted,omitempty"`
	// AdmissionReason is a short, bounded reason when not admitted:
	// MarkerMissing, MarkerStale, NotActiveHere, Quiescing, NoPrimaryHere,
	// NoInstanceHere, Closed, Unknown.
	// +optional
	AdmissionReason string `json:"admissionReason,omitempty"`
	// Healthy is false when the site's DC health Lease is stale or its
	// ManifestWork is not Applied and Available. An unhealthy site reports Ready
	// 0 and is never reported ready to the FailoverGroup.
	// +optional
	Healthy bool `json:"healthy,omitempty"`
	// ObservedAt is when this site's admission heartbeat was last seen.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
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
