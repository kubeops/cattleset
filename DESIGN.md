# CattleSet design

CattleSet is the application layer of KubeDB's stretched-hub DC/DR design. It runs one stateless application across the data centers named by a `PlacementPolicy`, as one object with one spec, and it follows the failover of the databases the application writes to.

This is the one current design document. The reasoning behind it, the review that shaped it, and the proposals made to other repositories are kept with the DC/DR project notes (`projects/dc-dr/cattleset-refresh/` in the prompt library).

## 1. Principles

1. **Ordering comes from dr-controlplane, unchanged.** An application is its own FailoverGroup that `dependsOn` the groups of the databases it writes to. CattleSet takes part in that machinery. It never invents an ordering mechanism of its own.
2. **Admission is decided on the spoke, from local facts, and fails closed.** A site's Service gets endpoints only while the databases the application depends on are active and writable in that same cluster. No hub call is involved, and anything unreadable means unadmitted.
3. **Capacity is fail-static.** Missing information never scales a serving site down. Admission and capacity are different decisions and fail in opposite directions.
4. **The posture is derived, and only ever tightens by itself.** The spec declares both postures up front; a failover changes no field of it and so touches no Git.
5. **No new authority.** CattleSet never writes a Lease, a database object, or a FailoverGroup spec. It writes its own member entry in its own group's status.

## 2. Shape

```
                 hub (stretched; ManagedCluster "hub", Arbiter DC)
   +----------------------------------------------------------------+
   | dr-controlplane: topology controller, FailoverGroup controller |
   | KubeDB: PetSet controller, Postgres orchestrator               |
   | CattleSet hub controller (HA, leader elected)                  |
   |   renders ManifestWorks, reports its FailoverGroup member,     |
   |   honors switchover-to, requests the app Lease handoff         |
   +-------------+--------------------------------+-----------------+
                 | ManifestWork                   | ManifestWork
          spoke-1 (DC A)                    spoke-2 (DC B)
   +--------------------------+     +--------------------------+
   | ns orders (no slice)     |     | ns orders (no slice)     |
   |   Deployment             |     |   Deployment             |
   |   Service (gated)        |     |   Service (gated)        |
   | ns demo (DB, on slice)   |     | ns demo (DB, on slice)   |
   |   Service pg-docs        |     |   Service pg-docs        |
   |   DB pods (role label)   |     |   DB pods (role label)   |
   | dc-failover: markers     |     | dc-failover: markers     |
   | gate agent (2 replicas)  |     | gate agent (2 replicas)  |
   +--------------------------+     +--------------------------+
```

- **Decides** the Leases and the FailoverGroup ordering: dr-controlplane.
- **Follows**, on the hub: the CattleSet controller, for capacity, reporting and the planned handoff request.
- **Follows**, on each spoke: the gate agent, for admission only, from local objects only.

A stateless CattleSet does not use KubeSlice. The application reaches its database through the database's own Service in the same cluster, so there is no cross-DC data path from the application, and a standby DC's application has no database to talk to. That is exactly what the gate encodes. The application therefore runs in its own namespace, not the database's: the database namespace is on the slice, and the slice would inject its sidecar into anything deployed there.

## 3. Modeling an application

```yaml
apiVersion: apps.k8s.appscode.com/v1
kind: FailoverGroup
metadata: { name: orders-app }
spec:
  dependsOn: [pg-docs]          # the database's own group
---
apiVersion: apps.k8s.appscode.com/v1
kind: PlacementPolicy
metadata: { name: orders-app-geo }
spec:
  clusterSpreadConstraint:
    distributionRules:           # the same Member and Arbiter DCs as the database
      - { clusterName: spoke-1, replicaIndices: [], role: Member }
      - { clusterName: spoke-2, replicaIndices: [], role: Member }
      - { clusterName: hub,     replicaIndices: [], role: Arbiter }
    failoverPolicy:
      mode: TwoDC
      failoverGroupRef: { name: orders-app }
```

- The user creates the FailoverGroup. The controller never writes its spec.
- A database an application gates on must have its own `failoverGroupRef`: `dependsOn` names groups, and the global scope has no group.
- The application's Member DCs must include wherever the database group can be active, or its group goes `Blocked`.
- The application must not be a member of the database's group; the controller flags that (`OrderingConfigured=False`, reason `AppSharesDataGroup`).
- `replicaIndices` is not meaningful for an application and is left empty. CattleSet does not call the PlacementPolicy validation that requires it for database Members.

## 4. Admission

The gate agent runs on every spoke. For each herd Service that carries the admission selector term, it decides from the gate spec the hub rendered into the Service annotation `apps.k8s.appscode.com/gate` and from local objects.

For a database `D` in group lease `L`, on spoke `S`:

| Check | Local source | Purpose |
|---|---|---|
| C1 active here | marker ConfigMap `dc-failover/<L>`: `activeDC == S`, `renewTime` within 30 s | The database fence's own rule. Closes the gate within 30 s of losing the coordination plane. |
| C2 not quiescing | marker `quiesce` does not name `S` | A planned switchover has frozen writes. |
| C3 writable here | the database's local Service has a ready endpoint; fallback: a Ready pod of the database with `kubedb.com/role=primary` | The database is serving at the address the application uses. |
| C4 break-glass | ConfigMap `dc-failover/<L>-override` exists on `S` | The operator forced this DC writable without the hub. The application follows. |

`admit = (C1 or C4) and C2 and C3`, for every tier.

- The marker alone is not enough: the Lease moves about 40 s before the database is writable. The role label or endpoint alone is not enough either: it outlives a stale marker by 30 to 60 s. Both are required.
- A marker renewed in the future by more than 10 s is distrusted, and the window can only shrink (`admission.markerMaxAge` above 30 s is rejected). `renewTime` is stamped in another DC and compared with the local clock, so NTP across DCs is a requirement.
- **Hysteresis.** Losing only C3 while the marker still names this DC is held for `admission.primaryLossGrace` (15 s by default), because a primary pod failure inside a DC is healed by local raft in 4 to 6 s and flapping the endpoints would also flap the drain label and the group's ready bit. Marker loss, another DC named, and quiesce are never graced. Recovery admits at once.
- **Camps.** What "active" means depends on the engine. Only Postgres is documented and tested. Leaderless engines (Cassandra, Weaviate, YugabyteDB) need only a local ready instance. Every other engine, including async-mirror and quorum engines and anything not in the table, gets the full set of checks. The default for an unknown kind is the restrictive one.
- **Draining.** On revoke the agent sets the pod label `apps.k8s.appscode.com/serving=false` on the herd's pods in that site (and `true` on admit) so an application can close long-lived sessions through the downward API. A pod created between two labeling passes carries no label until the next, so an absent label means unknown, never serving. No pod is restarted.

### What the gate guarantees

- While a gate agent runs on the spoke, admission fails closed: unknown means unadmitted. The Service is created with the unmatched selector term and applied with ServerSideApply and `ignoreFields` on `spec.selector`, so the agent is the selector's single writer.
- If no agent is running, the last admission state persists. Mitigation: two replicas with leader election and a PodDisruptionBudget, and a `GateAgentStale` condition when a site's heartbeat annotation goes silent.
- **Data safety never depends on the gate.** It rests on the database fence and on the database's local Service having no endpoints in a DC that is not active. The gate protects ordering and user experience: it keeps users from being routed to a site that cannot serve.

### What the hub renders

`mode` is `Tiers` (AND over the tiers), `AppMarker` (the herd's own marker must name this DC), or `Closed` (never admitted). A herd with nothing to depend on renders a Service with no admission term at all. An absent, unparsable, or unknown-version annotation, or `Tiers` with no tiers, all mean `Closed`. **No dependency members is never read as "nothing to wait for"**: it is also exactly what the controller sees before the database operator has reported, or when the FailoverGroup is missing, so those render `Closed`.

## 5. Posture

`topology.mode` is `Auto` (the default), `ActivePassive` or `ActiveActive`. `status.derivedMode` reports the result.

| Situation | Derived mode | Gate |
|---|---|---|
| No `failoverGroupRef`, explicit `ActiveActive` | ActiveActive | none |
| The group exists, `dependsOn` empty | ActiveActive under `Auto`; ActivePassive if explicit | none, or `AppMarker` |
| Any dependency is a single writer, async mirror, quorum engine or unknown | ActivePassive | `Tiers` |
| All dependencies are leaderless | ActiveActive | `Tiers` (a local instance is ready) |
| `Auto`, and the group is missing or a dependency has no members | ActivePassive | `Closed` |

- An explicit `ActivePassive` is always allowed. An explicit `ActiveActive` over a non-leaderless dependency is rejected by the webhook when the dependencies resolve, and otherwise tightened by the controller with `OrderingConfigured=False`, reason `ModeConflict`.
- The derived mode only tightens by itself. Once `ActivePassive`, it stays until the CattleSet spec changes, so a dependency whose members disappear cannot open a gate.
- There is no standby-DC serving: a standby site is hot but unadmitted. If cross-DC serving is ever wanted it needs a database-side cross-DC endpoint and its own design.

### Capacity per site (ActivePassive)

| Rule | Why |
|---|---|
| **Scale up early.** A Member gets `replicas` as soon as it holds the herd's Lease, or holds the Lease of any dependency group, or is admitted. | The dependency's Lease moves about 40 s before the database is writable. A warm or cold standby uses that window to start pods. |
| **Scale down late.** A Member drops to `standbyReplicas` only when the herd's group is `Steady` on another DC. | Never remove capacity on a guess. |
| **Hold on unknown.** If the holder cannot be established (an unreadable or expired Lease), keep the last computed counts and set `CapacityHeld`. | An unreadable Lease used to turn every site into a standby and scale the serving DC down. Fail-closed is for admission, not capacity. |

For a hot standby all three rules are no-ops; they matter for warm and cold standbys.

## 6. The FailoverGroup member

The controller applies one entry per CattleSet to its group's `status.members[]`, server-side, with its own field manager. It refreshes `observedAt` at least every 60 s, far inside the five minute `MemberReportTTL`.

`ready` is reported for the DC where the herd's own Lease is held, and is the conjunction of:

1. **healthy**: the DC's health Lease is fresh and the site's ManifestWork is Applied and Available. A dead DC is never ready, whatever its stale feedback still says.
2. **capacity**: available replicas at least `admission.minReadyReplicas`.
3. **serving**: the site is admitted, **or** it is unadmitted only because a direct dependency is active on another DC.

The third clause is what makes planned switchovers work. The FailoverGroup controller asks for a switchover only when every member is ready on the current Lease holder. Once the database has moved, the application on the old holder is correctly unadmitted; if that counted as not ready, the switchover would never be requested and the application's group would wait forever. On the target the dependency is local, so ready still requires real admission and `Steady` keeps its strict meaning.

## 7. Planned switchover and failback

A planned move arrives as `dr.kubedb.com/switchover-to=<dc>` on the CattleSet, set by the FailoverGroup controller or by a user. The handler is driven only by annotations and status, so a leader change resumes it:

1. **Validate.** The target must be a Member DC. With dependencies, it must be where they are active; otherwise the request is rejected (event `SwitchoverRejected`) because the group could never converge.
2. **Wait for capacity** on the target: `minReadyReplicas` available (immediate with a hot standby).
3. **Request the handoff.** Set `handoffTo: <target>` on the herd's own member entry. CattleSet does not touch the Lease. The FailoverGroup controller, which already annotates Leases, sets `handoff-to` on the group's Lease once every member of the group asks for the same DC and the target is a Member, alive, and where the dependencies are.
4. **Finish.** When the Lease holder is the target, remove `switchover-to` and withdraw `handoffTo`.
5. **Abort or timeout.** On `dr.kubedb.com/switchover-abort`, or after 10 minutes, withdraw `handoffTo` and remove the annotation. Removal on every exit is mandatory: the FailoverGroup controller leaves a member that already carries `switchover-to` alone, so a leftover annotation means the group can never be asked again.

A planned application move costs pod readiness plus one Lease handoff; there is no data to catch up. Admission does not wait for the application's Lease: the Lease is the ordering device, the gate is the safety device.

Failback is a planned database switchover. A returning DC has its pods up and its database Service empty, so it is unadmitted by construction, and the application follows through the same path with no manual step.

## 8. Failure behavior

| Event | Database | Application tier |
|---|---|---|
| Hub controllers down; spokes up | Unchanged | Admission keeps working from local state. Capacity and reporting freeze at last values. After five minutes the member report expires, which only blocks planned moves. |
| One spoke loses the coordination plane | Marker stale at 30 s, write-lock at about 40 s | That site's gate closes at 30 s, about 10 s before the database locks. |
| Both DCs lose it | Both write-lock | All gated sites close. Herds with no gate keep serving. Break-glass on one DC reopens that DC. |
| Intra-DC primary failure | Healed by local raft in 4 to 6 s | The gate holds through the grace; the application sees connection errors for those seconds and nothing else moves. |
| Aborted switchover | Writes resume on the original DC | The gate reopens when the quiesce clears. |
| `DCDRPromotionStalled` | Promotion held by the loss budget | Nothing is admitted in the survivor, `DataTierStalled=True`. The application never overrides the budget. |

Gated applications go dark when their database goes read-only, although reads could continue. A degraded read-only mode is possible later.

## 9. Repairs and lifecycle

- **Pod dependencies.** `spec.syncResources` lists ConfigMaps, Secrets and ServiceAccounts in the CattleSet's namespace on the hub. They go to every Member in a second ManifestWork per site (`<name>-deps`), which also creates the namespace and leaves it in place on delete (other workloads may share it). Updates are copied and pods are not restarted. A source that is missing on the hub keeps its copies on the spokes and sets `DependenciesSynced=False`; nothing is deleted because of a read. A ServiceAccount is copied by name, labels and annotations only. A synced Secret is stored inside a ManifestWork on the hub, so whoever can read ManifestWorks in that cluster namespace can read it; not listing the Secret keeps an external secret store available.
- **Deletion.** A finalizer removes the herd's ManifestWorks and withdraws its member entry. ManifestWorks of clusters that are no longer Members are pruned.
- **Autoscaling.** An HPA on the hub object cannot see pods that live on spokes. Version 1 supports only external-metric scalers (KEDA and similar) on the hub object writing `spec.replicas`. Per-site HPAs rendered into the ManifestWork are later work.
- **Lease watching.** Leases are watched on the coordination plane and only a holder or annotation change is an event, mapped only to the herds that use that Lease (their group, a dependency's group, or a site's health Lease). Plain renewals are ignored; expiry is caught by a timer set to the earliest expiry.
- **Adoption.** On the flip to distributed, a Member cluster adopts its local Deployment in place. When the controller runs in the Arbiter hub (`--cluster-name` names a cluster that is not a Member), nothing is adopted and the hub-local objects are removed once a Member serves. The webhook warns about a non-distributed CattleSet on an Arbiter hub (`--arbiter-hub`).
- **Rollouts.** Sites are ordered by observed admission, so the serving site is always last. A site's patch is part of its revision, so a patch-only change is sequenced like any other. Template propagation is held during a switchover or an unsettled failover; capacity and admission are never held.

## 10. Packaging

- **Hub controller:** two or more replicas, leader election on, no gate agent. It reads the coordination plane (`--coord-kubeconfig`, read only; empty means the hub's own API server) and needs no write access to Leases.
- **Gate agent:** its own Deployment on every spoke, two replicas with leader election, started with `--hub-controller=false --gate-agent --dc-name=<managed cluster name>`. It needs no hub credential. `--dc-name` is required: an agent that does not know which DC it is would never admit.
- Reference manifests for both are in `deploy/`. The Helm chart (`charts/cattleset` in `kubeops.dev/installer`) is generated from the same flags and RBAC.

Outside this repository, the design depends on two changes that are proposed to the owners of those repositories: petset's `FailoverGroupMember.handoffTo` and an optional `clusterSpreadConstraint.slice`, and dr-controlplane's handling of `handoffTo` (plus the permission to patch CattleSets, which the planned-switchover path needs). Until they are released and pinned, planned moves of an application group cannot complete; unplanned failover, admission and capacity do not need them.

## 11. Networking

Endpoint admission is the base layer: it composes with a GSLB health check, with route health injection for anycast, and with a mesh's endpoint discovery. The application layer owns whether a DC may receive traffic and publishes that state; it does not own the steering mechanism. `traffic.network` and `traffic.signal` are reserved placeholders and only `None` is accepted. Cross-DC application-to-application traffic is not provided: a tier and everything it calls synchronously are admitted in the same DC.

## 12. What is verified, and what is not

Verified by unit tests against fakes: the admission function and its hysteresis (one case per check), the closed-by-default parsing, the agent against a fake spoke (marker going stale mid-run, restart during a loss, the 15 s grace, break-glass, quiesce), ordering resolution and the derived mode, capacity holding on a read error, the ready definition and the full planned-switchover trace, the switchover handler, the finalizer, pruning, pod-dependency delivery, the Lease event filter, the webhook rules.

Verified on a real API server by `hack/e2e-kind.sh` (one kind cluster as hub and spoke-1, both roles deployed from `deploy/`, and dr-controlplane's FailoverGroup controller built from its `dc-dr-refresh` branch; 51 checks): the RBAC of both roles, leader election, server-side apply of the herd's member entry and its release on delete, the gate agent admitting, revoking on a stale marker (27 s after the last renewal), on another DC named, and after the 15 s primary-loss grace, riding out a 4 s primary change, break-glass, capacity held with both Leases gone, the old holder staying ready after the database moved, the FailoverGroup controller asking for the switchover, the herd's `handoffTo`, dr-controlplane writing `handoff-to` and removing its companion, the group reaching `Steady` on the target, and the finalizer. That run **plays** the OCM work agent, dr-controlplane's per-DC agents (including the Lease release and acquire of a handoff) and the database, and has no spoke-2, so it is not the joined DC-DR path.

Not verified, and needing the lab or a kind-and-OCM environment:

- That the `<db>` Service delivered to a standby spoke has no ready endpoint, and which pod labels select one database's pods (the C3 fallback uses `app.kubernetes.io/instance`).
- That a pod in a namespace outside the slice can reach the database's ClusterIP Service.
- That the OCM work agent resolves the JSONPath feedback `.metadata.annotations['<key>']` for an annotation key containing dots and a slash. If it does not, the hub sees no admission and every gated site reads as unknown.
- The joined planned-move path end to end: the herd's `handoffTo`, the FailoverGroup controller moving the Lease, the agents handing over.
- Timings: database writable to application admitted, and database group active to application Lease moved. The figures in this document are estimates from loop intervals.
- Every camp row other than Postgres. No source shows a dependent Redis or MongoDB end to end, and Redis has no camp, so it is a single writer.
