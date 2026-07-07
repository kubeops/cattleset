# Design Doc: CattleSet, a DC/DR-Native Stateless Workload API

**Status:** Draft for review
**Author:** (with research assistance)
**Date:** 2026-07-07
**Audience:** KubeDB / AppsCode engineering
**Repo:** `kubeops.dev/cattleset` at https://github.com/kubeops/cattleset (a new repo; API group `apps.k8s.appscode.com/v1`, shared with `PetSet` and `PlacementPolicy`, whose types it imports from `kubeops.dev/petset`)
**Companions:** `generic-app-dc-dr-design.md` (Case A and the A-front-tier of Case C), `kubedb-dc-failover-service-design.md` (the primary-DC Lease), `kubedb-dc-dr-design.md` (the database tier a herd usually fronts)

---

## 1. Summary

CattleSet is to Deployment what PetSet is to StatefulSet: the same workload contract, made data-center aware. A CattleSet runs one stateless application across the data centers named by a `PlacementPolicy`, as one object with one spec. The controller renders a plain `apps/v1` Deployment into each Member data center (via OCM `ManifestWork`, exactly like distributed PetSet), watches the shared primary-DC Lease from the Common Cross-DC Failover Service, and continuously realizes the declared posture: **ActiveActive** (every Member serves, anycast in front) or **ActivePassive** (the Lease holder serves, the other Members hold a warm standby).

The design's central property: **the spec is posture-invariant.** A CattleSet declares both postures up front (active capacity, standby capacity), and the controller flips between them by following the Lease. Failover therefore requires no spec edit and no Git commit, which structurally eliminates the GitOps drift-revert problem for the workload tier (`generic-app-dc-dr-design.md` Section 8.1): GitOps manages the CattleSet, the controller owns the rendered per-site Deployments, and no field is ever written by both.

CattleSet is also a Deployment when DR is not in play. With `distributed: false` (the default) the same object is spec-compatible with Deployment (the spec core mirrors `apps/v1` `DeploymentSpec` field for field) and manages a single local Deployment with identical semantics: no PlacementPolicy, no Lease, no multi-cluster machinery. Teams adopt one workload API everywhere, and a running herd becomes a DC/DR deployment later by flipping `distributed: true` and attaching a PlacementPolicy, a day-2 spec change rather than a migration. PetSet set this precedent: a StatefulSet replacement first, distributed when asked.

Pets are named and irreplaceable; cattle are interchangeable and managed as a herd. A PetSet gives each pod a stable identity and storage; a CattleSet manages an interchangeable herd across data centers.

## 2. Motivation

The generic-app DC/DR design (Case A, and the stateless front tier of Case C) tells users to run identical Deployments in both DCs via GitOps overlays, scale them per site, steer with anycast or k8gb, and hand-wire the failover posture. That works, but every piece is manual and per-application: two Deployments to keep in sync, per-site replica counts to flip at failover, a route or DNS object to repoint, and the sharp edge that a stateless front end must not admit writes in DR before its database is promoted (`generic-app-dc-dr-design.md` Sections 9 and 15.1). KubeDB already solved the stateful half with PetSet plus `PlacementPolicy` plus the failover service. CattleSet completes the pair, so a whole application (herd in front, pets and databases behind) is declared with the same vocabulary, follows the same Lease, and fails over as one unit.

## 3. Goals and non-goals

### Goals

1. One namespaced CRD, `CattleSet`, in `apps.k8s.appscode.com/v1`, that deploys a stateless workload across the clusters of a `PlacementPolicy` `clusterSpreadConstraint` and keeps the per-site Deployments identical by construction.
2. Both postures: ActiveActive (all Members serve; a lost Member's share is absorbed by survivors) and ActivePassive (the primary-DC Lease holder serves; standbys are warm).
3. Consume the Common Cross-DC Failover Service exactly as the database tier does: the `failoverPolicy.trigger` on the referenced `PlacementPolicy` names the Lease; a `holderIdentity` change is the failover; fail closed when the Lease is unreadable.
4. Enforce the write-path invariant at the workload: a herd that fronts a single-writer data tier is not admitted traffic in a site (its Service selects no endpoints) until that site's data tier is promoted (promotion strictly precedes traffic admission).
5. Reuse PetSet's conventions and patterns: `Distributed` + `ManifestWork` rendering (the pattern ported into `kubeops.dev/cattleset`), `PodPlacementPolicy` reference (types imported from `kubeops.dev/petset`), upstream `apps` types for strategy and status, scale subresource.
6. Coordinated cross-site rollouts with bounded global unavailability, and per-site status aggregation.
7. Drop-in Deployment compatibility: the spec core mirrors `apps/v1` `DeploymentSpec` field for field, and `distributed: false` behaves exactly like a Deployment (one local Deployment, HPA passthrough, standard rollouts), so adoption is a copy of an existing Deployment spec and enabling DR is a later flip, not a rewrite.

### Non-goals (v1)

1. **Stateful workloads.** No `volumeClaimTemplates`, and the webhook rejects PVC volumes in the pod template. Stateful means PetSet (or a KubeDB Database).
2. **Owning the traffic layer.** The controller renders Services and HTTPRoutes and maintains the GSLB primary tag; Gateways, BGP advertisement, and load balancers remain site infrastructure (`generic-app-dc-dr-design.md` Section 7).
3. **A per-herd failover quorum.** The decision is only ever the shared primary-DC Lease; CattleSet never runs its own election.
4. **Cross-herd orchestration.** Sequencing a herd with its database at failover belongs to the application DR umbrella (`kubedb-dc-dr-implementation-plan.md` Section 2X); CattleSet only contributes its gate and its posture-following.
5. **Multi-region beyond the PlacementPolicy's clusters, drill/test posture, and traffic mirroring.** Later.

---

## 4. API

### 4.1 Types (Go, following `petset_types.go` conventions)

The compatibility contract: **the spec core is `apps/v1` `DeploymentSpec`, field for field** (names, types, json tags, and defaults, per `k8s.io/api` `apps/v1/types.go`), followed by a DC/DR extension block in which every field is optional. A Deployment's `spec` therefore pastes into a CattleSet unchanged, a `FromDeployment` helper converts mechanically, and a CI round-trip test against the vendored upstream type keeps the mirror honest.

```go
const (
    ResourceKindCattleSet = "CattleSet"
    ResourceCattleSet     = "cattleset"
    ResourceCattleSets    = "cattlesets"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector

// CattleSet runs a stateless application across data centers as one unit.
// It renders one Deployment per Member data center and follows the
// primary-DC Lease selected by the referenced PlacementPolicy.
type CattleSet struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   CattleSetSpec   `json:"spec,omitempty"`
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
    // constraints (zone and node spread, affinity), the same ones PetSet
    // honors; a clusterSpreadConstraint then requires Distributed.
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
    // site HTTPRoute, and GSLB.
    // In non-distributed mode only ServiceName and HTTPRoute are allowed.
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

type CattleRolloutOrder string

const (
    CattleRolloutStandbyFirst  CattleRolloutOrder = "StandbyFirst"
    CattleRolloutOneSiteAtATime CattleRolloutOrder = "OneSiteAtATime"
    CattleRolloutParallel      CattleRolloutOrder = "Parallel"
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
    // local Service. Routes are steering, never the fence: the admission
    // gate is what keeps an unadmitted site's Service endpointless.
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
```

Status:

```go
type CattleSetStatus struct {
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // CurrentPrimaryDC mirrors the followed Lease's holderIdentity
    // (empty for ActiveActive without a trigger).
    CurrentPrimaryDC string `json:"currentPrimaryDC,omitempty"`

    // Selector is the serialized label selector, published for the
    // scale subresource so HPA can match the herd's pods.
    Selector string `json:"selector,omitempty"`

    // Aggregates across sites (scale subresource reads Replicas).
    // These mirror the apps/v1 DeploymentStatus aggregate fields, so
    // tooling that reads a Deployment's status reads a CattleSet's.
    Replicas            int32  `json:"replicas,omitempty"`
    UpdatedReplicas     int32  `json:"updatedReplicas,omitempty"`
    ReadyReplicas       int32  `json:"readyReplicas,omitempty"`
    AvailableReplicas   int32  `json:"availableReplicas,omitempty"`
    UnavailableReplicas int32  `json:"unavailableReplicas,omitempty"`
    CollisionCount      *int32 `json:"collisionCount,omitempty"`

    Sites []CattleSiteStatus `json:"sites,omitempty"`

    // Conditions: Available, Progressing, PostureConverged,
    // FailoverInProgress, WritePathGated, LeaseAccessible.
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    LastFailoverTime *metav1.Time `json:"lastFailoverTime,omitempty"`
}

type CattlePosture string

const (
    CattlePostureActive  CattlePosture = "Active"
    CattlePostureStandby CattlePosture = "Standby"
    CattlePostureNone    CattlePosture = "None" // any non-Member DC (Witness collapses into Arbiter for a herd)
)

type CattleSiteStatus struct {
    ClusterName string      `json:"clusterName"`
    Role        DCRole      `json:"role"`
    Posture     CattlePosture `json:"posture"`
    Desired     int32       `json:"desired"`
    Ready       int32       `json:"ready"`
    Updated     int32       `json:"updated"`
    Revision    string      `json:"revision,omitempty"`
    // Gated is true while the write-path gate holds this site's
    // Service unadmitted (data tier not promoted here).
    Gated bool `json:"gated,omitempty"`
}
```

### 4.2 Example: the front tier of an orders application

The herd and its Postgres share one `PlacementPolicy` group, so one Lease moves both.

```yaml
apiVersion: apps.k8s.appscode.com/v1
kind: PlacementPolicy
metadata: { name: orders-geo }
spec:
  clusterSpreadConstraint:
    distributionRules:
      - { clusterName: dc-a, replicaIndices: [0, 1], role: Member }
      - { clusterName: dc-b, replicaIndices: [2, 3], role: Member }
      - { clusterName: dc-c, replicaIndices: [],     role: Arbiter }
    slice: { projectNamespace: kubeslice-prod, sliceName: orders-slice }
    failoverPolicy:
      mode: TwoDC
      trigger: { scope: Group, group: orders }        # lease primary-dc-orders
---
apiVersion: apps.k8s.appscode.com/v1
kind: CattleSet
metadata: { name: orders-api, namespace: orders }
spec:
  distributed: true
  replicas: 6
  selector: { matchLabels: { app: orders-api } }
  template:
    metadata: { labels: { app: orders-api } }
    spec:
      containers:
        - name: api
          image: harbor.local/orders/api:2.4.1
  sitePatches:
    - clusterName: dc-b
      patch: { spec: { template: { spec: { containers: [ { name: api, image: harbor-b.local/orders/api:2.4.1 } ] } } } }
  podPlacementPolicy: { name: orders-geo }
  topology:
    mode: ActivePassive
    standbyReplicas: 6            # hot standby, RTO = traffic cutover
  rolloutOrder: StandbyFirst
  traffic:
    serviceName: orders-api
    export: true
    httpRoute: { parentRefs: [ { name: public-gw, sectionName: https } ] }
    gslb: { host: orders.bank.example, ttlSeconds: 30 }
  gates:
    dataTiers:
      - { apiGroup: kubedb.com, kind: Postgres, name: orders-pg }
```

Non-distributed form, a Deployment in all but name:

```yaml
apiVersion: apps.k8s.appscode.com/v1
kind: CattleSet
metadata: { name: orders-api, namespace: orders }
spec:
  replicas: 3
  selector: { matchLabels: { app: orders-api } }
  template:
    metadata: { labels: { app: orders-api } }
    spec:
      containers:
        - name: api
          image: harbor.local/orders/api:2.4.1
```

Enabling DR later is a spec change (`distributed: true`, `podPlacementPolicy`, `topology`), not a new object: the controller adopts the local Deployment as the local site's rendered output and seeds the other sites.

The stateless-only rule is enforced, not assumed: no `volumeClaimTemplates` field exists, and the webhook rejects a pod template that mounts a PersistentVolumeClaim (emptyDir, configMap, secret, projected, and ephemeral CSI volumes are fine).

---

## 5. Posture semantics

### 5.1 Non-distributed (a Deployment)

With `distributed: false` the CattleSet owns exactly one Deployment (plus the `traffic.serviceName` Service and its HTTPRoute if set) in its own namespace and cluster: no ManifestWork, no Lease, no gate, no posture. Replicas, strategy, `minReadySeconds`, `progressDeadlineSeconds`, `paused`, and the scale subresource pass straight through (a literal field copy, the spec core being `DeploymentSpec`), so behavior is a Deployment's, including rollout and revision semantics (the ReplicaSet history lives on the owned Deployment). A referenced PlacementPolicy contributes only its local constraints (zone and node spread, affinity). This mode exists so one workload API serves everything from a dev cluster to a two-DC bank deployment, and so DR is a day-2 flip rather than a migration.

In distributed mode, the controller computes each site's desired replica count from (mode, roles, the Lease, per-DC health). Pods run only in Member DCs; every other DC gets zero pods. There is no Witness distinct from Arbiter here: Witness exists for data-bearing engines (a replica that must hold data yet never lead), and a herd holds no data, so a PlacementPolicy `Witness` rule is treated exactly as `Arbiter`, a non-serving vote.

### 5.2 ActiveActive

Every Member serves at once. Desired per Member = its weighted share of `replicas` (even split default, `topology.distribution` to skew). Traffic is the anycast or roundRobin GSLB pattern (`generic-app-dc-dr-design.md` Sections 7.1 and 11 Case A); nothing is single-writer, so no Lease is required and no fencing exists. When a Member DC is lost (its per-DC health Lease goes stale, or its `ManagedCluster` is unavailable), survivors scale up by `surgeOnFailover` of the lost share. This is capacity, not correctness: traffic already moved at the BGP or DNS layer. When the site returns, its Deployment resumes its share and the surge is withdrawn.

If a trigger is configured anyway, the holder only orders GSLB answers; it never gates serving.

### 5.3 ActivePassive

The Lease holder Member runs `replicas`; every other Member runs `standbyReplicas` (default equal to `replicas`, a hot standby); the webhook requires a `failoverPolicy` on the referenced PlacementPolicy. The controller follows the Lease with the failover service's client library: consistent read or watch, never a cached list, and **fail closed** (a site that cannot read or renew its view of the Lease treats itself as not-primary; the herd there stays in standby posture).

Unplanned failover, step by step (the workload-tier slice of `generic-app-dc-dr-design.md` Section 15.1):

1. The shared quorum moves `primary-dc-orders` from `dc-a` to `dc-b`.
2. The hub controller observes the holder change and re-renders: `dc-b` desired goes from `standbyReplicas` to `replicas` (hot standby: already there, a no-op), `dc-a` to `standbyReplicas`.
3. The write-path gate (Section 6) keeps `dc-b`'s Service unadmitted (no endpoints) until every referenced data tier reports `dc-b` as its promoted primary. Promotion strictly precedes traffic admission, enforced at the Service rather than by convention.
4. The controller updates the GSLB primary tag to `dc-b`; the per site HTTPRoutes need no repoint (each is bound to its local Gateway and Service, whose endpoints the gate controls); BGP advertisement follows site infrastructure health gating.
5. The `ManifestWork` for `dc-a` now carries standby posture. A dead `dc-a` simply does not apply it; when `dc-a` returns, OCM applies it on arrival, so **a returning site comes back in standby posture by construction**, and its gate holds (its local data tier is demoted, so its Service stays unadmitted), so it cannot attract writes even if stale DNS or routes still point at it. Steering remains steering; the data tier's Lease and quorum remain the fence.

Failback is the same mechanism in reverse, triggered by the coordinated Lease handoff (`kubedb-dc-failover-service-design.md` Section 7); the herd needs no special path.

### 5.4 Control plane loss

The controller runs on the OCM hub (HA, witness site). If the hub is down, every site keeps its last applied posture: an ActiveActive herd still fails over at the traffic layer (BGP withdrawal needs no controller), and an ActivePassive herd's standby stays gated until its data tier is promoted, so nothing unsafe happens; what is lost is posture convergence (scale-up, GSLB retag) until the hub returns. This is the same availability class as the database DR controller and is acceptable because correctness never depends on the herd controller, only capacity and steering polish do.

---

## 6. The write-path gate

The sharpest stateless-DR bug is a front end that admits traffic in DR before its database is promoted (`generic-app-dc-dr-design.md` Sections 9 and 15.1). CattleSet closes it at **traffic admission**, not at pod readiness:

- The rendered Service of a gated herd starts **unadmitted**: its selector carries a controller-owned admission term (`apps.k8s.appscode.com/admitted: "true"`) that no pod matches, so the Service selects no endpoints.
- The spoke gate agent (local to the site, hub-independent) **admits** the site by patching that term out of the Service selector, and revokes it by restoring the term. It admits only while **every** `gates.dataTiers` reference reports this site as its promoted, writable primary (from the Database's `status.disasterRecovery`, which the DR driver maintains from the same Lease). Admission is spoke-local, so a hub outage never blocks a legitimate admission or forces a wrong one. One wiring requirement makes the flip stick: a Service `selector` is an atomic map, and the ManifestWork applier would otherwise reapply the rendered selector on resync and undo the agent's flip, so the Service's ManifestWork entry uses `updateStrategy: ServerSideApply` with `ignoreFields` on `spec.selector`, making the gate agent the selector's single writer after creation (the same single-writer rule, applied to OCM).
- Ungated herds (no `gates`, truly external or read-only backends) skip the gate; their Services render admitted.

Why not a pod `readinessGate`: a standby site's pods would be permanently not-Ready, which wedges everything that consumes readiness. A rolling update on the standby could never progress (new pods never become Ready, so old ones are never scaled down), `progressDeadlineSeconds` would report the site failed, and PodDisruptionBudgets would see zero healthy pods. Gating endpoint membership instead leaves pods Ready, so rollouts, PDBs, and availability math behave normally on a standby, while every steering layer is still cut off at once: Service endpoints, HTTPRoute backends (routes reference the Service), slice export, GSLB health, and ClusterMesh. The ClusterMesh caveat (a pure L4 health decision with no knowledge of the data primary) is neutralized the right way around: an unadmitted site has no local endpoints, so a global service with `affinity: local` falls back to the active site's backends, which is exactly where the traffic belongs.

The webhook requires gated data tiers to follow the same `failoverPolicy.trigger` as the herd. Fail over together or gate forever: a herd in group `orders` gated on a database in group `payments` would deadlock after any split decision, so it is rejected at admission. Gating also requires `topology.mode: ActivePassive`: write-path gating implies a single writer, and an ActiveActive herd whose standby is gated shut is ActivePassive in disguise.

---

## 7. Rollouts, availability, autoscaling

**Rollouts.** A template change rolls out per site with the standard Deployment strategy, sequenced by `rolloutOrder`: `StandbyFirst` (default for ActivePassive) updates standby sites and waits for them to become ready before touching the active site (the admission gate does not touch pod readiness, so standby rollouts complete normally); `OneSiteAtATime` (default for ActiveActive) bounds the blast radius to one site's share; `Parallel` for dev. All sites converge to one revision except mid-rollout, and `status.sites[].revision` exposes skew.

**Availability.** The controller renders a per-site PodDisruptionBudget from the site's desired count and the strategy's `maxUnavailable`. `minReadySeconds`, `progressDeadlineSeconds`, and `paused` behave as in Deployment, per site.

**Autoscaling.** The scale subresource targets `spec.replicas` (the active capacity), so HPA and KEDA attach to the CattleSet, and the controller fans the new capacity out per posture (in non-distributed mode this is a direct passthrough to the one Deployment). Attaching an HPA to a rendered per-site Deployment is unsupported and reverted: the controller is the single writer of per-site replica counts (the same single-writer rule as GitOps, applied to autoscalers).

**GitOps.** Users commit the CattleSet (and PlacementPolicy) to Git. Because posture is derived from the Lease, not from spec fields, a failover changes no Git-managed object and drift correction never fights the controller. The rendered Deployments are controller-owned outputs, not Git content.

---

## 8. Validation and defaulting (webhook)

- `template` must be stateless: reject PVC volume sources; no `volumeClaimTemplates` field exists.
- `ActivePassive` requires the referenced PlacementPolicy to carry `failoverPolicy` (a trigger to follow); `ActiveActive` requires at least two `Member` rules.
- `topology.distribution` and `sitePatches` may name only clusters present in the `distributionRules`, and `distribution` only Members; `sitePatches` may not touch `replicas`, `selector`, labels used by `selector`, or the admission gate.
- `gates.dataTiers` must resolve, expose DR status, and carry the same `failoverPolicy.trigger` as the herd; `gates` requires `topology.mode: ActivePassive` (Section 6).
- `selector` and `topology.mode` are immutable in v1; `standbyReplicas` defaults to `replicas`; `surgeOnFailover` defaults to `100%`; the Deployment core keeps upstream defaults (`replicas` 1, `revisionHistoryLimit` 10, `progressDeadlineSeconds` 600); extension defaults apply only when `distributed: true` (defaulting a forbidden field on a non-distributed object would trip the webhook's own validation).
- `distributed: true` requires `podPlacementPolicy` (with a `clusterSpreadConstraint`) and `topology`. `distributed: false` requires the absence of `topology`, `sitePatches`, `rolloutOrder`, `gates`, `traffic.export`, and `traffic.gslb`; a referenced PlacementPolicy may then carry only local constraints.
- Flipping `distributed` false to true is the supported DR-enable path (the local Deployment is adopted as the local site's rendered output, then the other sites are seeded); true to false is rejected in v1.

## 9. Implementation sketch

1. **Types and codegen** in the new `kubeops.dev/cattleset` repo, `apis/apps/v1/cattleset_types.go` (the `PlacementPolicy` types are imported from `kubeops.dev/petset`, not redeclared), scheme registration, CRD manifests, deepcopy, clients; scale subresource as in PetSet. Add a `FromDeployment` converter and a CI round-trip test that pins the spec core to the vendored `apps/v1` `DeploymentSpec`, so an upstream field change fails the build instead of silently diverging. The template is upstream `core.PodTemplateSpec` (byte compatible with Deployment), not PetSet's pruned local copy.
2. **Hub reconciler** (in `kubeops.dev/cattleset`): resolve the PlacementPolicy, compute posture (Section 5), render one Deployment (+ Service, PDB, optional HTTPRoute, Gslb, and ServiceExport) per Member into a `ManifestWork` per cluster, with feedback rules projecting `readyReplicas`, `updatedReplicas`, and `availableReplicas` back; aggregate into `status.sites` and the totals. In non-distributed mode the same renderer emits the one local Deployment directly (no ManifestWork), and enabling distribution adopts it.
3. **Lease follower:** the failover service client library (`OnPrimaryDCChanged`, `IsThisDCPrimary`), one informer per trigger shared across CattleSets; hysteresis re-uses the service's `leaseDurationSeconds`, no second grace knob.
4. **Spoke gate agent:** watch the local Database DR status and admit or revoke the local Service (the admission selector term) for gated herds in that cluster; admission is spoke-local, so a hub outage never blocks it.
5. **Webhook** per Section 8; **e2e** on the three-cluster harness (implementation plan Section 4): posture flip on Lease move, returning-site-comes-back-standby, gate-holds-until-promotion, surge on Member loss, StandbyFirst rollout, non-distributed Deployment parity, and the false-to-true distribution flip with adoption.

## 10. Open questions

1. **Session draining on posture flip:** how aggressively to drain the old active site's long-lived connections (the anycast reconvergence question, `generic-app-dc-dr-design.md` Section 15.4), and whether CattleSet should own a preStop drain default.
2. **Drill posture:** a test-failover mode that activates the standby site against a branched data tier (KubeDB cross-cluster branching) without moving the Lease; natural v2 tie-in to the drill harness.
3. **Weighted ActiveActive plus HPA:** when HPA scales `replicas`, are `distribution` weights re-normalized continuously or on a hysteresis, to avoid per-site thrash?
4. **Disabling distribution:** the true-to-false collapse (which site's Deployment survives, how traffic drains); rejected in v1 until specified.

## 11. References

- CattleSet repo: https://github.com/kubeops/cattleset (module `kubeops.dev/cattleset`)
- apps/v1 DeploymentSpec, the mirrored core: https://github.com/kubernetes/api/blob/master/apps/v1/types.go
- PetSet types and conventions: `kubeops.dev/petset` `apis/apps/v1/petset_types.go` (`Distributed`, `PodPlacementPolicy`, upstream `apps` reuse, scale subresource).
- PlacementPolicy with DC roles and failover trigger: `kubeops.dev/petset` `apis/apps/v1/placementpolicy_types.go` (`ClusterSpreadConstraint`, `DistributionRule.Role`, `FailoverPolicy`, `FailoverTrigger`).
- The failover signal: `kubedb-dc-failover-service-design.md` (primary-DC Lease, per-DC health Leases, client library, coordinated failback handoff).
- The stateless DR patterns this implements: `generic-app-dc-dr-design.md` Sections 4.1, 7, 8.1, 9, 11 (Case A), 15.1.
- The data tier a herd fronts: `kubedb-dc-dr-design.md` (Database `status.disasterRecovery`, promotion, fencing).
- OCM ManifestWork: https://open-cluster-management.io/docs/concepts/work-distribution/manifestwork/
- k8gb failover strategy: https://www.k8gb.io/docs/strategy.html
- Gateway API HTTPRoute: https://gateway-api.sigs.k8s.io/api-types/httproute/
