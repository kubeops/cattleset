# Design Doc: Generic Application DC/DR on Kubernetes

**Status:** Draft for review
**Author:** (with research assistance)
**Date:** 2026-06-24
**Audience:** Platform engineering, for a regulated-industry (banking) cloud platform
**Companion:** `kubedb-dc-dr-design.md` covers the database tier in depth; this doc covers the whole application and its block, object, network, and delivery tiers, and reuses the same third-site witness model.

---

## 1. Summary

This document designs disaster recovery across two data centers (DC active, DR standby) for a generic application, not just its database. The same application runs in both data centers, traffic fails over to DR automatically when DC is lost, and the system fails back to DC when it recovers. The design is built around one decision that determines everything else: **is the application stateless (or backed by storage that is genuinely active/active), or is its storage active/passive (single-writer)?**

- **Stateless, or active/active storage:** both sites can serve at once. Steer users to the nearest healthy site with an **anycast VIP advertised by BGP** from both DCs, and let route withdrawal move traffic on failure. Cross-region or cross-provider, layer a DNS GSLB (k8gb) on top.
- **Active/passive storage (the common case for databases and most stateful apps):** only the active site serves writes. The application always writes to its **local** storage and database, and replication to the standby happens **below the application**, at the storage/CSI layer (DRBD, Ceph RBD mirror, or array-based replication) or the database layer (Postgres WAL, MySQL binlog). On a DC failure both the traffic and the storage primary move to DR together.

The hard part is not any single tier; it is that a real application is several stateful tiers at once (a database, one or more block volumes, and object storage), each replicating with its own independent lag, so at the failover instant they are mutually inconsistent unless you coordinate them. The design treats the application, not the volume, as the unit of failover, uses storage **consistency groups** for the block tier, points to the database DC/DR design for the database tier, and explicitly accepts and contains the eventual-consistency of the object tier.

The design reuses the third-site **witness** from the database DC/DR doc (a small third data center that holds no application data and exists to break quorum ties and host the failover decision-maker), and the concrete decision mechanism is the shared Common Cross-DC Failover Service's primary-DC Lease (Section 8.3), so automatic failover is safe and not split-brained. It is assembled from proven open-source building blocks (BGP via Cilium or MetalLB, Cilium ClusterMesh, k8gb, OCM/ACM (with Karmada optional), Argo CD or Flux, Ceph or DRBD or commercial arrays, NooBaa or Ceph RGW or MinIO for objects, Harbor for images), and it exposes a small set of options to the end user (RPO/RTO target, replication mode, topology, residency, failover governance, DR-drill mode) that map directly to banking regulations (BCBS 239, DORA, RBI, FFIEC, UK operational resilience).

---

## 2. Motivation and scope

A bank running on Kubernetes needs more than database HA. A whole application, its stateless API tier, its databases, its block-backed stateful services, its object storage (documents, statements, images), its container images, and the network in front of it, must survive the loss of an entire data center with a bounded, auditable recovery time and data loss. Regulators increasingly mandate this: DORA requires yearly resilience testing of critical functions on live systems, RBI requires near-zero RPO and an identical DR site for critical systems with periodic drills, and FFIEC and the UK operational-resilience rules require defined RPO/RTO (or impact tolerances) and evidence that you can stay within them.

In scope: the application tier (stateless and stateful), block/volume replication, object storage replication, traffic steering and client redirection, multi-cluster orchestration and GitOps delivery, the container registry, the third-site witness, failover and failback orchestration, and the end-user-facing options a regulated customer configures. The database tier's internal failover (promotion, quorum, the primary-DC Lease, fencing) is specified in the companion `kubedb-dc-dr-design.md` and is referenced here rather than repeated.

Out of scope: application code changes to become multi-master (we replicate below the app), the choice of a specific backup engine (Velero, Kasten, and so on, a companion concern), and more than one DR region in v1. Note that **immutable, point-in-time backup is in scope and required**, not optional: replication alone is not disaster recovery against corruption or ransomware (Section 6.6), so the architectural placement of an immutable copy is part of this design even though the backup product is not.

---

## 3. Goals and non-goals

### Goals

1. The same application runs in DC and DR; on full-DC loss, traffic and (for active/passive) the storage primary fail over to DR automatically and safely; when DC returns, the system fails back.
2. Cover both regimes: stateless / active-active (both sites serve, anycast + BGP), and active/passive (single-writer, replicate below the app, move together on failover).
3. Replicate every stateful tier: database (WAL/binlog, see companion doc), block volumes (DRBD / Ceph RBD mirror / array), and object storage (async bucket replication), each with an explicit, measured RPO.
4. Treat the application as the unit of failover: coordinate the block tier with consistency groups, and contain the cross-tier inconsistency that independent replication streams create.
5. Reuse the third-site witness so automatic failover is quorum-safe and never split-brained.
6. Assemble from proven open-source projects with commercial-array options, and run the decision-maker and image supply in each site so a DC outage does not disable DR.
7. Expose a small, regulator-aligned set of options to the end user, with maker-checker governance, scheduled non-disruptive DR drills, residency pinning, and immutable audit evidence.

### Non-goals (v1)

1. **Synchronous everything at any distance.** Synchronous block and database replication is RPO zero but metro-distance only; across regions the design is asynchronous with a bounded RPO. We do not pretend otherwise.
2. **Application-level active/active for single-writer apps.** We do not rewrite an app to multi-master; if it is single-writer, it is active/passive and writes locally.
3. **Cross-tier (database + block + object) synchronous consistency groups.** No tool provides a consistency group spanning all three; we coordinate the block tier, reference the database tier, and make the app tolerant of object eventual-consistency rather than claim a global consistent point.
4. **A second DR region or active/active across three regions** beyond the witness. v1 is DC + DR + witness.

---

## 4. The core decision tree (the permutations)

Everything downstream is decided by the application's state model. There are three cases.

### 4.1 Stateless tier

No durable local state (or all state externalized to a replicated database/object store). Both DCs can run the app and serve traffic simultaneously. This is the easy case: run identical deployments in both clusters (GitOps), front them with an **anycast VIP advertised by BGP** from both sites so each user reaches the nearest one, and on a site failure let the BGP route withdraw so traffic flows to the survivor. Cross-region or cross-provider where a shared VIP is impossible, use a **DNS GSLB (k8gb)** instead or on top. Orchestration (OCM/ACM, or Karmada) can also reschedule the stateless pods onto DR automatically if a cluster is lost, though with anycast that is a capacity action, not a correctness one.

### 4.2 Active/active storage

The storage or data layer is itself designed to accept writes in both sites: a synchronously stretched array (Pure ActiveCluster, HPE Peer Persistence, Dell SRDF/Metro, Hitachi global-active device), a stretched Ceph cluster (Metro-DR), or an application-level multi-master data layer (an object store with bidirectional bucket replication, or a multi-master database). Then both sites serve reads and writes, and traffic steering is the same anycast/active-active pattern as the stateless case. The catch is that genuine active/active storage is **metro-distance only** for the synchronous variants (latency is paid on every write) and, for asynchronous multi-master, brings conflict resolution (last-write-wins) that the application must tolerate. Raw block is essentially never safely active/active across sites (see DRBD dual-primary below); active/active at the storage layer means a purpose-built stretched array or a higher-level data service, not two nodes writing the same block device. These are two different regimes that must not be conflated: **B1, synchronous stretched** storage (arrays, stretched Ceph) is RPO zero and strongly consistent but metro-distance only; **B2, asynchronous multi-master** (bidirectional bucket replication, a multi-master database) works at any distance but has a non-zero RPO and last-write-wins conflicts, so it is only safe for commutative or append-only data and is closer to Case C's async story than to B1.

### 4.3 Active/passive storage (the default for stateful apps)

One site is the writer; the other is a warm standby. This is the regime for most databases and most block-backed stateful services. Two rules define it:

1. **The application always writes to its local storage and database.** The app is unaware of DR; it never writes cross-DC.
2. **Replication happens below the application,** at the storage/CSI layer (DRBD, Ceph RBD mirror, array replication) or the database layer (Postgres WAL streaming, MySQL binlog), shipping changes from the active site to the standby continuously.

On a DC failure, the failover is not just the app: the storage/database **primary role** moves to DR (promotion), and only then does the app in DR start serving. Traffic and the data primary move together. This is exactly the database DC/DR design generalized to the whole stateful app, and it is where the witness, fencing, and the consistency-group problem all live.

A real application is usually a mix: a stateless API tier (case 4.1) in front of an active/passive database and block tier (4.3), plus object storage (often async-replicated, a fourth independent stream). The design composes the three cases per tier and coordinates their failover.

---

## 5. Failure domains and the witness (shared with the database design)

Two data centers cannot self-arbitrate an automatic failover: from DR's point of view a dead DC and a partitioned-but-alive DC are indistinguishable, so naive auto-promotion produces two writers and divergent data (split-brain). As in the database DC/DR doc, the design mandates a **third data center as a witness**: a small site that holds no application data, contributes a quorum vote, and hosts the failover decision-maker (the orchestration hub, the failover controller, and the Common Cross-DC Failover Service's arbiter etcd member, whose majority backs the primary-DC Lease). Any partition then resolves to a clear majority, the surviving side proceeds, and the old primary is fenced or self-fences. The synchronous stretched-storage options need the same third site for their arbiter (Ceph stretch mode requires an arbiter mon in a third location; sync arrays use a third-site witness or quorum witness). One third site therefore serves the whole stack, but its **quorum/arbiter** role is needed only where there is a single writer to arbitrate: the active/passive promotion decision (Case C) and the synchronous-stretch arbiter (Case B1). Pure stateless active/active (Case A) needs no quorum or arbiter, since its failover is just anycast withdrawal with nothing to fence; the third site still usefully **hosts the control plane** (the orchestration hub, the failover controller, the GitOps engine, the registry) for all cases. Fail-safe rule: if the witness itself is unreachable when DC fails, the system does **not** auto-promote (that would be guessing without quorum); it holds, keeps DC serving if DC is alive, and exposes the manual maker-checker override, failing safe rather than open.

Two layers of failover, kept separate so a cross-DC event is rare: intra-DC HA (local replicas, fast local failover) handles pod and node failures without any cross-DC action, and cross-DC DR (this document) triggers only on a full data center loss, decided by quorum across DC, DR, and witness.

The **cross-DC replication link is a first-class prerequisite and a failure domain.** Every stream in Section 6 (DRBD Proxy, RBD mirror, WAL and binlog, bucket replication) needs a WAN, VPN, or dedicated fabric sized for at least the peak write rate plus headroom; undersized bandwidth silently inflates the async RPO. Its loss is not benign: for **asynchronous** replication the RPO grows without bound until the link returns (and the send buffer or journal can fill and back-pressure the primary), while for **synchronous** replication a link loss stalls the primary's writes entirely until the mode degrades or the secondary is evicted from quorum. Both behaviors must be alerted on (growing lag, buffer fill, sync stall), and link loss is a distinct DR-drill scenario.

---

## 6. The data replication layer

The single most important principle: **the application writes locally and the data is replicated beneath it.** Three independent tiers replicate (block, database, object), each with its own mechanism and its own RPO. They are not mutually consistent at the failover instant unless coordinated (Section 6.5).

### 6.1 Block / volume replication

For PVCs (filesystems, queues, search indexes, anything not a managed database), the volume is replicated below the app. The fundamental trade is distance versus RPO: **synchronous replication gives RPO zero but only over metro distance** (every write waits for the remote acknowledgement), while **asynchronous replication works at any distance but has a non-zero RPO** equal to the lag.

**DRBD / LINBIT (https://linbit.com/drbd/), the reference open-source block replicator.** DRBD is kernel-level network RAID-1: it intercepts writes to a local device and ships them to a peer. Protocol C is synchronous (the local write completes only after the peer confirms it reached disk, RPO zero, metro only); protocol A is asynchronous (the write completes once it is in the local send buffer, RPO greater than zero, any distance). **DRBD Proxy** sits in the data path for protocol-A WAN replication, buffering and compressing the stream so a slow inter-site link does not stall the primary; the common production pattern is a stacked setup, synchronous within the active site and asynchronous over DRBD Proxy to DR. On Kubernetes, **LINSTOR** is the control plane and the **LINSTOR CSI driver** (deployed by the **Piraeus operator**, a CNCF sandbox project) provisions DRBD-replicated PVs. Important caveat: DRBD dual-primary (active/active block) requires a cluster filesystem (OCFS2/GFS2 with a distributed lock manager) plus fencing/STONITH, and without fencing a partition corrupts data, so active/active raw block is dangerous and not used for DR; the DR norm is active/passive protocol C or A. DRBD 9 adds a quorum feature (three nodes, a diskless arbitrator allowed) as a STONITH alternative, which fits the third-site witness.

**Other open-source options that you asked about.** The honest answer is that DRBD and Ceph RBD mirroring are the only two true cross-site block replicators in open source. The rest are different things: **Longhorn** replicates synchronously only within a cluster (intra-cluster HA), and its cross-site DR is backup-based (back up to S3, continuously restore into a standby DR volume), so its RPO is the backup cadence, not real-time block sync. **OpenEBS Mayastor** is intra-cluster NVMe-oF replication with no true cross-cluster block sync.

**Does Ceph support this? Yes, via RBD mirroring.** The `rbd-mirror` daemon asynchronously mirrors RBD images between two Ceph clusters. It is **async only** (there is no synchronous RBD mirror, so RPO is always greater than zero), in two modes: journal-based (every write goes to a journal then the image, near-real-time and crash-consistent, but roughly doubles write latency) and snapshot-based (periodic mirror snapshots, cheaper on the write path, RPO bounded by the snapshot interval). It is one image primary, one non-primary (read-only), so no active/active; failover is promote/demote. **Rook** integrates it on Kubernetes. For synchronous Ceph (RPO zero) you instead run a single **stretched** Ceph cluster across two metro sites with an arbiter in a third site (Ceph stretch mode), which is the basis of OpenShift Metro-DR.

**The Kubernetes-native standard: csi-addons VolumeReplication + Ramen.** The de-facto-standard CRD for storage replication is the **csi-addons `VolumeReplication`** API (`replication.storage.openshift.io`), which references a PVC and a `VolumeReplicationClass` and carries a `replicationState` of primary, secondary, or resync that the CSI driver executes. **`VolumeReplicationGroup` (VRG)** groups an app's PVCs so they replicate and fail over together (the consistency-group primitive). **Ramen**, under **Open Cluster Management / Red Hat ACM**, is the controller that orchestrates this and is the engine behind Red Hat OpenShift DR. This is the closest existing Kubernetes-native reference architecture for storage DR, and this design follows the same CRD shape. We adopt the `VolumeReplication`/`VolumeReplicationGroup` model for orchestration, but with an important caveat (Section 6.5): the group's write-order consistency is a property of the **backend**, not of the Kubernetes grouping, so the model is not replicator-independent for the consistency guarantee.

**Commercial storage arrays** (for customers who already run them, exposed through their CSI drivers and a replication module):

| Vendor | Sync (RPO 0, metro) | Async (any distance) | Active/active | Kubernetes integration |
|---|---|---|---|---|
| Dell PowerMax SRDF | SRDF/S | SRDF/A, RecoverPoint CDP | SRDF/Metro (+ Smart DR to a 3rd site) | Dell CSI + CSM Replication |
| NetApp ONTAP | SnapMirror Sync | SnapMirror async | No (active/passive) | Trident CSI (TridentMirrorRelationship) |
| Pure Storage | ActiveCluster (RPO 0 + RTO 0) | async replication | ActiveCluster; Portworx PX-DR sync/async | Portworx PX-DR (k8s-native) |
| HPE Primera/Alletra/Nimble | Peer Persistence | Remote Copy async | Peer Persistence (auto switchover) | HPE CSI driver |
| IBM Spectrum Virtualize | Metro Mirror | Global Mirror | HyperSwap | IBM block CSI driver |
| Hitachi VSP | TrueCopy | Universal Replicator | global-active device | Hitachi HSPC CSI driver |

Array-based replication is mature; the gap to verify per vendor is the maturity of their CSI-driven, app-group-consistent Kubernetes failover orchestration (Ramen/ODF and Portworx are the most turnkey here).

### 6.2 Database replication

Databases replicate at the database layer, not the block layer, because the engine understands its own write order: Postgres WAL streaming, MySQL/MariaDB binlog, MongoDB oplog, and so on. This is the subject of the companion `kubedb-dc-dr-design.md`, which covers per-engine cross-DC replication (sync in-DC + async cross-DC by default), promotion, the third-site witness, the primary-DC Lease that self-fences a partitioned writer, fencing, and failback. For this document the database is one of the application's stateful tiers with its own RPO (typically the smallest of the three, often sub-second to seconds), failed over by that design. Do not also block-replicate a database's PVCs: pick the database-layer replication, which is consistent, over block mirroring, which is not aware of transactions.

### 6.3 Object storage replication

Most modern applications also need S3-compatible object storage (documents, statements, images, ML artifacts), and it must be replicated to DR too. The key fact: **object replication is asynchronous and eventually consistent in essentially every product**, so the object tier has its own, usually larger, RPO that is independent of the block and database RPOs.

- **NooBaa (https://www.noobaa.io), the Multicloud Object Gateway in OpenShift Data Foundation.** A Kubernetes-native S3 gateway over many backing stores. Its DR-relevant mechanism is **bucket replication**: asynchronous, unidirectional by default (active/passive, source to remote), and bidirectional (active/active) when a complementing policy is added on the second bucket, and the destination can be a second cluster (DC to DR). Log-based replication (driven by cloud bucket event logs) is available for namespace-backed buckets. Good fit when you want an S3 abstraction over heterogeneous backends; async, so non-zero object RPO.
- **Ceph RGW multisite, the answer to "does Ceph object storage have a DR mechanism?" Yes.** RGW multisite (realm, zonegroup, zone) asynchronously replicates objects and metadata between clusters. It is active/active for data writes (objects can be written at any zone) with a single metadata master per zonegroup (bucket and user creation routes to the master), and active/passive is a directional zone. Octopus and later add per-bucket sync policy (symmetrical or directional flows, prefix and tag filters) and implement the S3 replication API. **Rook** manages it on Kubernetes. This is the natural object tier when the block tier is already Ceph.
- **MinIO, a major Kubernetes-native option.** Two models: bucket replication (per-bucket, one-way or two-way active/active, multi-site mesh) and, the right tool for full DC/DR, **site replication**, which is active/active and synchronizes everything (all buckets and objects plus IAM users, policies, service accounts, and bucket config), which NooBaa and Ceph do not auto-sync the same way. It is asynchronous by default ("near-synchronous" with adequate bandwidth); a synchronous per-target mode exists but still returns success whether or not the replica attempt succeeded, so it is not RPO zero. Plan for a small but non-zero object RPO and front the sites with a load balancer or GSLB.
- For reference, **Zenko/Scality** does asynchronous multi-cloud replication (one-to-many) via the S3 API, and cloud **S3 Cross-Region Replication** is the canonical model (asynchronous, with Replication Time Control giving an SLA of 99.99% of objects within 15 minutes, never zero).

### 6.4 The three RPOs are different, and that is the point

A failover combines three independently lagging streams: the database (often sub-second to seconds), the block volumes (seconds to minutes, depending on journal vs snapshot mirroring), and the object store (seconds to minutes, sometimes more). They are not the same number and they are not coordinated. The platform must measure and surface each separately (database replication lag, `VolumeReplication` status, Ceph `sync status` or MinIO/RTC pending metrics) and the workload's effective RPO is the worst of the three.

### 6.5 The consistency-group problem (the most important cross-cutting issue)

A real application is rarely one volume. It may have a database, multiple PVCs (for example data and write-ahead-log on separate disks), and an object bucket. For a correct DR failover, the parts that reference each other must be captured at a single write-order-consistent point. Two distinct hazards:

1. **Within the block tier:** if two PVCs of one application (or two disks of one LVM-spanned filesystem) replicate independently, their async streams advance at different rates, so at the failover instant volume A may be at a later point than volume B. If A holds a log and B holds the data the log describes, the standby comes up internally inconsistent or corrupt. The fix is **consistency groups**: replicate and promote the app's PVCs as one unit at a common write-order-consistent point. Crucial precision: write-order consistency across volumes is a **backend** property, not a Kubernetes-grouping property. `VolumeReplicationGroup` orchestrates grouped promotion, but it only delivers a consistent point if the underlying replicator supports consistent group snapshots or group mirroring: **Ceph `rbd group` mirror snapshots** (a shared journal or single group snapshot across images) or an **array consistency group** (Dell SRDF consistency group, and similar). **DRBD replicates each volume independently and provides no cross-volume write-order consistency**, so grouping two DRBD-backed PVCs in a VRG promotes them together but they were never captured at a common point and the standby can still come up inconsistent. Therefore, on DRBD, place co-dependent data on a **single** replicated volume, or use a backend with native consistency groups (Ceph group mirroring, array CG). Recent OpenShift DR makes multi-volume consistency groups the default (on Ceph) and adds a Recipe object that declares which PVCs and Kubernetes resources belong to an app and in what capture/restore order. The unit of block replication is the application's group, never the individual volume, but the guarantee is only as strong as the backend underneath.

2. **Across tiers (database + block + object):** no tool provides a consistency group spanning all three, and they each have a different RPO (6.4). At the failover instant a database row can point to an object that has not yet replicated, or a PVC can reference a not-yet-replicated blob, a dangling reference. The design does not pretend to solve this with a global consistent point; instead it contains it: (a) treat each tier's RPO as an explicit, measured number and size the workload's tolerance to the worst; (b) make object storage **versioned and write-once / immutable** (enable versioning and object lock) so late-arriving replicas never clobber and a missing object is simply not-yet-present rather than wrong; (c) make the application **tolerant of temporarily missing objects** (retry, repair-on-read, reconcile jobs) instead of assuming referential integrity at the failover moment; and (d) for a **planned** switchover, quiesce and checkpoint so all three tiers reach a common point before the flip (the planned-switchover path is zero-RPO; the unplanned failover path is not). This is the honest boundary of multi-tier DR and must be stated to the customer, not hidden.

### 6.6 Replication is not backup: the corruption and ransomware boundary

Every replication mechanism in this section (DRBD, RBD mirror, WAL and binlog, bucket replication) faithfully and promptly ships whatever the active site wrote to DR within the RPO, including an erroneous bulk write, a malicious `DROP`, a logic bug, and ransomware encryption. Replication therefore protects against losing a data center; it provides no protection against the corruption and ransomware disaster class, because the bad write reaches DR before anyone notices. For a regulated bank this is the threat that matters most, and FFIEC states the requirement directly: data protection is a combination of backup, replication, and storage, and offline backups must be protected from malware that may corrupt both the production and the online copies.

A complete posture needs both, independently:

- **Replication** gives low RPO and RTO against site or infrastructure loss (this document).
- **Immutable, point-in-time backups** give recoverability against logical corruption and ransomware, which replication cannot. At least one copy must be **write-once (object-lock / WORM) or air-gapped**, with a retention lock no principal (including an administrator or a compromised control plane) can delete, held far enough back in time to predate an undetected corruption, and stored **outside the blast radius of both the DC and DR control planes**, since an attacker who reaches production must not be able to reach the immutable copy. This is the converse of the point in Section 14: backup is not a substitute for replication, and replication is not a substitute for immutable backup.

The backup product (Velero, Kasten, and so on) is a companion choice, but the requirement and the placement of the immutable copy are part of this design, and the DR drill (Section 12) must include restoring from the immutable backup, not only failing over the replica. Two constraints on the immutable copy interact: it must sit outside the blast radius of both control planes yet stay inside the permitted jurisdiction, so for a residency-constrained customer it is an in-country write-once object store rather than an out-of-region one; and it must be decryptable after the disaster, so its encryption key must survive the same event (Section 12, key custody).

### 6.7 Shared or stretched storage, and cross-cluster branching

A recent KubeDB capability is worth positioning explicitly: cross-cluster branching with shared storage, where a copy-on-write branch in one cluster clones a source in another because both clusters reach the same storage. Whether it helps DR depends entirely on what "shared" means, and the failure-domain rule decides it.

- **A single shared storage system** (one Ceph cluster, one array, one networked target reachable from both clusters) is a **correlated failure domain, not a DR site.** It adds a compute cluster, not an independent copy: if the storage or its site fails, both clusters lose the data. It is a multi-cluster convenience, not DR, and it does nothing for any-distance DR, since you cannot share storage across regions (latency forbids it).
- **Stretched, synchronously mirrored storage** across a metro pair with an arbiter in a third site (Ceph stretch mode, Pure ActiveCluster, and similar) is genuine metro DR (Case B1). Here shared storage changes the failover shape usefully: DR already holds a synchronous copy, so failover collapses from "replicate then promote" into "fence, then attach and promote," with no separate database replication stream. It remains metro-distance only, and storage-level fencing becomes mandatory (below).

What cross-cluster branching adds in both readings:

- **Cheaper DR drills.** Spin up a near-production branch in the DR cluster by copy-on-write instead of a full restore, run a non-disruptive test failover against real-shaped data, validate integrity, and tear it down. This is the strongest practical benefit and feeds the drill harness (Section 12).
- **Faster DR seeding.** Where storage is shared or stretched, the bootstrap that establishes DR can clone rather than copy a full base backup.

The caveat it introduces: **storage-level fencing is now essential.** With replication, a partitioned old primary writes to its own independent storage (divergent but isolated). On shared storage, two clusters writing the same blocks is corruption, not divergence. A branch is safe because copy-on-write isolates its writes to new blocks, but a failover that promotes DR onto the same volume the old primary held needs hard storage fencing (SCSI reservations, the array's own fencing, or the copy-on-write origin pin), on top of the primary-DC Lease and quorum fence.

Bottom line: cross-cluster branching with shared storage strengthens the **metro** path (B1) and is a strong tool for DR testing and seeding, but it does not replace the independent-storage-plus-replication path (Case C) for any-distance DR, and the witness and quorum are still required.

---

## 7. Traffic steering and client redirection

Two layers move clients to the right site: an L3 anycast layer for fast local and intra-region failover, and a DNS GSLB layer for cross-region or cross-provider failover. They compose.

### 7.1 Anycast IP + BGP (your active/active path)

Advertise the **same VIP from both DCs via BGP**. Routers run BGP best-path and, on the tie-break, send each user to the nearest site (geographic locality), with ECMP load-balancing equal paths by per-connection hash. This is how stateless and active/active traffic is served close to the user. On a site failure the route is withdrawn (BGP session drop, or health-gated withdrawal) and every router reconverges to the survivor; detection is fastest with BFD (sub-second), then aggressive hold/keepalive timers, then a graceful TCP FIN on clean shutdown.

In Kubernetes the VIP is advertised by **MetalLB** (BGP mode, now defaulting to the FRR-K8s backend with BFD and graceful restart) or, preferred when Cilium is the CNI, the **Cilium BGP Control Plane** (with LB-IPAM assigning the VIP). Both are strictly per-cluster: two instances in two DCs simply both originate the VIP, and the active/passive intelligence lives in BGP attributes plus health-gated advertisement, not in the tool.

**The single-writer caveat you raised.** Pure equal-cost anycast sends each user to the nearest site, which is correct for stateless and active/active but wrong for a single-writer (active/passive) service where all writes must land on the active site. To make DR a true backup, de-prefer the standby's route so it is used only when the primary withdraws: **LOCAL_PREF** is strongest but iBGP-only (use it when you own both sites in one AS), **AS-path prepend** is the standard cross-AS (eBGP) lever, and **communities** tag routes for an upstream you do not control to de-prefer. The decisive limitation: BGP/ECMP hashing is stateless and the hash is not stable across membership changes, so when the backend set changes (a site withdraws or restores), existing flows are re-hashed and most in-flight TCP connections break once (clients reconnect and resolve to the survivor). Anycast failover is therefore not transparent to long-lived stateful sessions; mitigate with resilient/stable ECMP on the routers, a stateful L7 ingress behind the anycast VIP to hide backend churn, graceful-shutdown for planned cutovers, and client retry. (Scope health checks to local-only signals so a shared dependency cannot make every node withdraw at once.)

**The BGP/anycast and DNS layer is steering, not fencing.** A partitioned-but-alive DC keeps advertising its VIP (its health checks are local-only, by design), so the traffic flip alone cannot stop writes to the old primary: clients still routed to DC would still write there. The write-stop is always the **data-layer fence** (the primary-DC Lease expiry or quorum loss makes the old primary read-only, per the companion design); traffic steering only minimizes client misdirection and is irrelevant to write safety. So for active/passive, advertise the VIP primarily from the active site (or strongly de-preferred from DR) and flip the advertisement on failover as a client-experience optimization, after the data-layer fence and promotion have made writes safe; for active/active, advertise equally from both with resilient ECMP and an L7 layer in front.

### 7.2 k8gb (DNS GSLB) for cross-region or cross-provider

When the two DCs are in different providers or networks with no shared L3, a shared BGP VIP is impossible; use **k8gb (https://www.k8gb.io)**, a CNCF-sandbox, Kubernetes-native global server load balancer built on CoreDNS and external-dns. It is **decentralized with no single point of failure**: each cluster runs its own k8gb and CoreDNS and is authoritative for its own records, stitched together by DNS zone delegation, so there is no central controller or shared database. It health-checks via Kubernetes readiness and steers with strategies including `roundRobin` (active/active), `weightRoundRobin`, and `failover` (active/passive, pinned to a `primaryGeoTag`: only the primary's IPs resolve while it has a healthy pod, and on total loss DR's ingress IPs are returned, with automatic failback on recovery). The caveat is DNS: failover is gated by TTL (default 30s) plus client and resolver caching, so it is seconds-to-minutes class and only redirects new resolutions, not in-flight connections.

**Combine them (the recommendation):** BGP anycast inside each region for instant local failover (same stable VIP, sub-second reconverge), and k8gb DNS GSLB between regions for the geo failover. Each region advertises its own anycast VIP internally; k8gb publishes the global hostname and returns Region A's VIP while healthy, failing the DNS answer over to Region B's VIP only on a full-region loss, without needing BGP between regions or providers.

---

## 8. Orchestration, GitOps delivery, and the failover decision

### 8.1 Keeping both sites identical: GitOps (Argo CD or Flux)

GitOps is the right way to keep DC and DR identical and to make the active-site choice an auditable decision, and either of the two CNCF-graduated engines, **Argo CD** or **Flux**, does the job. The requirement is the same regardless of tool: one Git repo as the source of truth, one application spec deployed to both clusters with per-site deltas (active vs passive replica counts, local endpoints, the local registry), continuous drift self-heal so the sites cannot silently diverge, and a failover or failback that is recorded as a **Git commit** (flip a `role: active|passive` value, scale DR up and DC down, change the route-weight object), which is auditable (author, diff, timestamp), reversible (failback is a revert), and identical-by-construction. The critical caveat holds for both: GitOps reconciliation is configuration convergence, not an instant data-plane switch (reconcile is minutes by default, faster with webhooks, then pods must become healthy), so **pair the Git change with the fast data-plane cutover (BGP withdrawal or k8gb DNS) for the actual traffic switch**. The engine is control-plane, not data-path, and is rebuildable from Git, so run it HA in the witness site or per-cluster.

The two engines map feature-for-feature; the differences are idiomatic, and none of them touch the DR correctness machinery (witness, lease, fencing, promotion), which lives entirely below GitOps:

- **Multi-cluster fan-out:** Argo CD uses an **ApplicationSet (cluster generator)** to template one app to both clusters from a hub; Flux uses per-site **Kustomize overlays** (each cluster reconciles its own path) or targets a remote cluster via a `kubeConfig` on the Kustomization or HelmRelease. Same outcome.
- **Reconciliation model:** Flux is typically one agent per cluster (decentralized pull), so each site keeps reconciling from Git with no central GitOps control plane to lose, a slight DR positive. Argo CD is often one instance managing several clusters (a rebuildable control plane), which is why it is run HA in the witness.
- **Progressive, metric-gated promotion:** **Argo Rollouts** for Argo CD, **Flagger** for Flux. Equivalent.
- **Image automation:** built into Flux (the image-automation-controller); an add-on (Argo Image Updater) for Argo CD.
- **UI and ecosystem:** Argo CD has the stronger built-in UI and, more important here, the tighter fit with the recommended OCM/ACM control plane and the OpenShift GitOps ecosystem this design benchmarks against, which is why **Argo CD is the default, not a requirement**. A bank standardized on Flux uses it with no change to the DR design; the only non-technical difference is commercial support and stewardship (both are CNCF-graduated and vendor-neutral; enterprise support for Argo CD comes from Akuity, Codefresh, and Red Hat, and for Flux mainly from ControlPlane).

Two operational points bite GitOps DR specifically, under either engine. First, the GitOps engine **reverts any imperative change to a field it manages** (drift correction), so the failover-mutable fields (the active/passive role, replica counts, the route-weight object) must have a **single writer**: either the failover controller commits them to Git (the source of truth, reconciled in minutes, which is acceptable because the fast traffic cutover is handled separately by BGP and DNS), or those specific fields are excluded from GitOps reconciliation (Argo CD `ignoreDifferences`, Flux drift exclusion) and the controller owns them imperatively for speed. Never let both the controller and GitOps write the same field, or the engine will undo the failover. Second, an **automatic** failover cannot wait for a human pull-request review, so the controller pushes the active-site commit unattended (with scoped Git credentials) and it is audited after the fact; the human gate, where policy requires one, is the maker-checker approval on the promotion itself (Section 12), not a Git review. Planned switchover and failback, by contrast, can and should go through a reviewed commit.

### 8.2 Multi-cluster orchestration: OCM/ACM or Karmada

The platform needs a multi-cluster control plane to place workloads and react to a cluster loss. Two good options, and the choice should match the rest of the platform.

- **Open Cluster Management (OCM) / Red Hat ACM** is the hub-spoke, pull-based model the companion database design and KubeDB Platform already use (the hub registers DC and DR as spokes, `Placement` selects clusters, failover is taint-and-reselect, delegated to a workload applier such as Argo CD or Flux). It is strong on governance, policy, and observability, which regulated customers value, and keeps one control plane across the database and application tiers. This is the recommended default for consistency with the existing platform.
- **Karmada (https://karmada.io)** is the alternative when you want a real multi-cluster scheduler with automatic stateless rescheduling on cluster loss. Its `PropagationPolicy` supports `replicaScheduling: Duplicated` (full count in each cluster, natural for active/active) and `clusterAffinities` (ordered primary/backup, for active/passive), and `OverridePolicy` with `ImageOverrider` makes the DR site pull from its local registry. Its **cluster failover** (taint-based eviction and reschedule) automatically moves stateless workloads off a lost cluster; note the `Failover` feature gate is Beta and off by default, and `StatePreservation` carries only a state handle, not the data. The honest limit, shared by both: they move workloads, not data, so stateful failover still rides on the data-replication tiers (Section 6) and the database design.

**OCM/ACM is the standard control plane here, consistent with KubeDB Platform, and it also hosts the failover service's primary-DC Lease (Section 8.3). Karmada stays useful for the stateless tier and as the auto-reschedule engine on DC loss, but it does not replace the data-replication, lease, and witness machinery, so it is an optional add-on, not the default.**

### 8.3 The failover controller

As in the database design, the cross-DC decision is made once by the shared **Common Cross-DC Failover Service** (`kubedb-dc-failover-service-design.md`): a 3-DC etcd quorum, one member per data center including the witness arbiter, exposed as a standard `coordination.k8s.io` primary-DC Lease through an OCM control plane. A small failover controller runs in the witness site (on the OCM hub) and consumes that Lease rather than re-deciding: it watches the primary-DC Lease, and on a change of holder (or a Lease the active site can no longer renew) it drives the storage/database promotion (Section 6, companion doc), flips the traffic advertisement (BGP and k8gb), reconciles the active-site change through GitOps (Section 8.1), and orchestrates failback. The whole-application controller does not re-decide per tier; it reads the one shared Lease and sequences the tiers (database, block, object, traffic) under that single quorum decision. Promotion of a single-writer tier is gated by the same fencing and primary-DC Lease rules as the database design, so a partitioned old primary, unable to hold the Lease, self-fences and cannot keep writing.

---

## 9. Network layer: Cilium

**Can Cilium help? Yes, at two layers.** First, **Cilium ClusterMesh** connects the two clusters into one mesh by extending the eBPF datapath across cluster boundaries (not a gateway proxy), giving east-west pod-to-pod connectivity with network-policy enforcement. Its DR-relevant feature is **global services with service affinity**: deploy a service with the same name in both clusters, mark it global, and set `service.cilium.io/affinity: local` so it load-balances across healthy local backends and uses the remote site only when all local backends are unhealthy, giving **local-first L4 service failover at the datapath, sub-second, with no DNS change**. Set a finite `cacheTTL` so a fully-down cluster's stale backends are revoked. This is the best active/active local-first primitive when both DCs run Cilium with L3 connectivity between them. Second, the **Cilium BGP Control Plane** advertises the LoadBalancer VIP for the anycast layer (Section 7.1), so a Cilium-based platform gets both north-south anycast and east-west mesh from one stack. The requirements are the usual mesh constraints (non-overlapping pod CIDRs, unique cluster IDs, node-to-node connectivity, shared CA). ClusterMesh handles the stateless/service plane; it does not replicate stateful data, which remains the data-tier's job. One sharp caveat: ClusterMesh `affinity: local` is a pure L4 health decision with **no knowledge of which site is the data primary**, and its local-to-remote failover is sub-second, faster than BGP or DNS. It must therefore be confined to the stateless/service plane and **must not front the write path of a single-writer (Case C) service**, where it could route or split writes to DR's not-yet-promoted instance ahead of, and independent of, the fence-and-promote sequence. For Case C the write entry point follows the promoted data primary, not ClusterMesh affinity.

---

## 10. Container image supply: Harbor

**Can Harbor be used? Yes, and it should be, one per data center with replication.** A DC outage must not block DR from pulling images, so each site runs its own **Harbor (https://goharbor.io)** registry and they replicate to each other. Harbor's policy-based **replication** syncs artifacts and their Cosign signatures between registries, push-based (active pushes to DR) or pull-based, with manual, scheduled, or **event-based** triggers (replicate immediately on push). Run event-based replication for fast propagation with a scheduled full sync as a backstop, keep both Harbors on the same major version (cross-major-version replication is not supported), and make workloads resolve to the **local** registry (pinned references or a containerd registry mirror) so there is no cross-DC pull dependency at failover. Harbor's **proxy cache** is a complement for upstream third-party base images (lazy pull-through, serves the cached copy when upstream is unreachable), but for your own images use eager replication, not proxy cache, since proxy cache only holds images already pulled. Keep each Harbor internally HA (external HA Postgres and Redis, shared/object storage); cross-site survivability comes from per-DC Harbor plus replication, not from stretching one Harbor across both sites. Harbor also gives the governance a regulated customer expects (RBAC, OIDC, Trivy scanning, signing).

---

## 11. Reference architectures

The pieces assemble into reference shapes from the Section 4 cases. All host their control plane (hub, controller, GitOps engine, registry) in the third site and use GitOps (Argo CD or Flux); they differ in the data and traffic tiers and in whether the third site also plays a **quorum/arbiter** role (only B1 sync-stretch and C promotion do; A needs none).

**A. Stateless (or fully externalized state).** Identical deployments in both DCs via the GitOps engine (Argo CD ApplicationSet or Flux Kustomize overlays); OCM/ACM or Karmada places and reschedules them; **anycast VIP advertised equally from both DCs** (Cilium BGP or MetalLB) with resilient ECMP and an L7 ingress in front; Cilium ClusterMesh global services with `affinity: local` for east-west; k8gb on top for cross-region. No data tier to fail over, and no quorum or arbiter needed (nothing to fence or promote). RPO and RTO are effectively zero; failover is automatic and transparent except for in-flight connections, which break once and reconnect.

**B1. Synchronous stretched storage (metro).** As A for the app and traffic, plus a storage layer that is genuinely active/active over a low-latency metro link: a stretched Ceph cluster (Metro-DR) with an arbiter in the witness, or a synchronous array (Pure ActiveCluster, HPE Peer Persistence, Dell SRDF/Metro). RPO zero, RTO near zero, strongly consistent, **metro-distance only**. Both sites serve reads and writes.

**B2. Asynchronous multi-master (any distance).** As A for the app and traffic, plus a multi-master database or bidirectional NooBaa/RGW/MinIO bucket replication. Any distance, but **RPO greater than zero and last-write-wins conflicts** the app must tolerate, so it suits commutative or append-only data, not arbitrary block. Closer in spirit to Case C's async story than to B1.

**C. Active/passive storage (any distance, the default for stateful apps).** Stateless tier as in A; the stateful tiers replicate below the app: the database via the companion design (WAL/binlog, sync in-DC + async cross-DC, witness, the primary-DC Lease, fencing), the block volumes via DRBD protocol A over Proxy or Ceph RBD async mirror as a **consistency group** (`VolumeReplicationGroup`, only as strong as the backend's group-consistency, Section 6.5), and the object store via async bucket replication (NooBaa, Ceph RGW multisite, or MinIO site replication). Only the active site serves writes; the app always writes locally. On full-DC loss the primary-DC Lease moves to DR and the witness-hosted controller fences the old primary (the data-layer fence, not the traffic flip, is the write-stop), promotes the storage and database primaries in DR (as a group), then flips the BGP advertisement and k8gb DNS to DR, and commits the active-site change to Git. RPO is the worst of the three tiers' lag (usually the object tier); RTO is the sum of detection (the primary-DC Lease duration, the failover-signal floor), per-tier group promotion, and traffic cutover (BGP reconverge in seconds, or DNS TTL plus resolver caching for the cross-region path, which dominates and is seconds-to-minutes), plus the in-flight reconnect. Failback is a planned, lower-RPO switchover after DC re-syncs.

Most real applications are **C with an A front tier**: a stateless API served active/active by anycast, in front of an active/passive database, block, and object back end that fails over as a coordinated unit.

---

## 12. What the end user (a regulated bank) provides and the options to expose

A regulated customer does not configure DRBD protocols or BGP communities; they declare intent and the platform realizes it. The inputs and the user-facing options below map directly to BCBS 239, DORA, RBI, FFIEC, and the UK and BIS operational-resilience rules.

The customer first classifies each workload by **criticality tier** (for example Tier-0 mission-critical down to Tier-3), which auto-derives the allowed RPO/RTO band, the default replication mode, and the drill cadence. Then, per workload, the platform exposes:

- **RPO target:** zero (sync), near-zero (seconds), minutes, or hours. Drives sync vs async and metro vs any-distance. (FFIEC RPO; RBI near-zero for critical systems.)
- **RTO target:** near-real-time, minutes, or hours, and stated for **two distinct recovery paths**, because they differ sharply: a **site-loss failover** (fast, the replication-promotion-plus-cutover path of Section 15) versus a **corruption or ransomware recovery** (restore from the immutable backup into a clean environment, dominated by restore time and materially slower). A single RTO number hides this; the customer sets and the platform proves both. (FFIEC RTO; maps to UK impact tolerance.)
- **Topology:** active/active (where state allows) or active/passive (hot, warm, or cold standby).
- **Replication mode:** synchronous (RPO zero, metro guardrail enforced) or asynchronous (any distance), with the platform refusing sync beyond a latency threshold and sizing the cross-DC link to the peak write rate. Note the standing cost: synchronous protocols pay a remote-acknowledgement latency on **every** write, and journal-based block mirroring roughly doubles write I/O continuously, so a low-RPO choice carries a permanent performance price, not only a distance constraint.
- **Witness / arbiter placement:** which third site holds the quorum vote and the failover controller, with a region selector so the witness honors residency.
- **Failover governance:** automatic, or manual-approval with a **maker-checker / two-person rule** and the recorded approver identity. (BIS operational-resilience governance and segregation of duties; RBI IT governance.)
- **Scheduled DR-drill / non-disruptive test-failover:** a production-safe test mode on a calendar (presets for quarterly, half-yearly, annual) with a scenario library (including WAN-link loss and a ransomware-restore-from-immutable-backup scenario) and an exportable result. The drill must **validate recovered data integrity, not just pod readiness**: after test-failover it runs database consistency checks and reconciliation to pre-drill control totals, cross-tier referential checks (database rows referencing objects or blobs resolve, per Section 6.5), and records the realized RPO per tier, so a drill that brings pods up on inconsistent data cannot pass. (DORA yearly testing of critical systems and three-yearly threat-led tests on live systems; RBI drills; FFIEC exercises; BCBS 239 data-integrity and reconciliation; UK and BIS severe-but-plausible scenario testing.)
- **Data residency / region pinning** for the primary, the replica, the backup, and the witness, so data and its copies stay in permitted jurisdictions. (RBI 2018 payment-data localisation requires data only in India; DORA jurisdictional control.)
- **Encryption and key custody:** customer-managed keys (BYOK / HSM) with key-region pinning, reinforcing residency for replicas and backups. The key material is itself a DR dependency and a single point of failure: if the KMS or HSM lives only in DC, the replicas and the immutable backup cannot be decrypted at DR or at the restore site after a DC loss, so the key store must be highly available across the same failure domains as the data (a replicated or quorum-distributed KMS, an HSM with a cross-site partner), and key availability is a required check in every DR drill.
- **Retention:** backup and journal / redo-log retention and the point-in-time-recovery window.
- **Audit-log and evidence export:** immutable, timestamped records of every failover and drill, and exportable test reports, to satisfy supervisory exams and the DORA reporting clocks (major incident initial notice within four hours, intermediate at seventy-two hours, final at one month).
- **ICT-register / third-party evidence hooks:** the platform, as an in-scope ICT provider, exports resilience-test results and recovery attestations for the customer's DORA register of information.

The platform's job is to translate these declarations into the concrete mechanisms of Sections 6 to 10 and to prove, with the drill and audit features, that the declared RPO/RTO are actually met, which is what regulators now require.

---

## 13. Responsibilities: the shared-responsibility model

DC-DR for a real application is delivered by three parties, and a clean split of who provides and who operates each layer is what makes the result auditable. It is also a regulatory expectation in its own right: the third-party and ICT-dependency rules (DORA Article 28, BIS operational resilience, FFIEC third-party) require the bank to know exactly which obligations sit with each provider and to hold evidence of it. The three actors:

- **Cloud / infrastructure provider** (a public cloud, a colocation provider, or the bank's own on-premise infrastructure team): the physical and virtual substrate, the sites, the network between them, compute, the storage systems and their replication capability, and object storage.
- **Platform stack provider (AppsCode / KubeDB Platform):** the software that turns those substrate primitives into automatic, safe, regulator-aligned DC-DR, the operators, the DC-DR control plane and failover controller, the shared primary-DC Lease (the Common Cross-DC Failover Service) and witness logic, the per-engine drivers, the traffic-steering and GitOps integration, and the drill, integrity, and evidence machinery.
- **End user (the bank):** the application and its data, the intent and policy (RPO/RTO, residency, governance), the approvals, and the regulatory relationship.

### 13.1 Who provides what

Bottom-up, each layer and the actor that **provides** it (the operate axis is Section 13.2).

| Layer / capability | Cloud / infrastructure provider | Platform stack provider (AppsCode) | End user (bank) |
|---|---|---|---|
| Sites and failure domains: DC, DR, and the third witness site, each an independent failure domain (power, cooling, physical security) | Provides all three sites as independent domains | Specifies that three independent domains are required | Chooses sites and jurisdictions (residency) |
| Inter-DC network: the WAN or interconnect link with a bandwidth and latency SLA; BGP peering and route advertisement; DNS and global-load-balancing substrate; load balancers and VIPs | Provides the link, the SLA, and the network primitives | Configures k8gb, the BGP control plane, Cilium ClusterMesh, and the GSLB on top of them | Specifies the bandwidth and latency the workload needs |
| Compute and Kubernetes clusters in each site | Provides nodes (and managed Kubernetes, if a managed offering) | Requires and validates the clusters; installs the operators | Self-runs the clusters on the infrastructure if not managed |
| Storage: block backend (Ceph or DRBD substrate, or commercial arrays with array replication), object storage service, CSI drivers, and the replication capability itself | Provides the storage, the CSI drivers, and the block/object replication capability | Orchestrates it through `VolumeReplication`/`VolumeReplicationGroup`, consistency groups, and the object replication engine | Chooses the storage class and tier |
| Immutable backup target (object-lock/WORM or air-gapped), and the KMS/HSM for keys | Provides the WORM object store, the air-gap, and the KMS/HSM service | Orchestrates backup, restore, and the restore drill | Sets the retention policy and owns key custody (BYOK) |
| DC-DR control plane: the failover controller on the OCM hub in the witness, the Common Cross-DC Failover Service (the 3-DC etcd quorum and the primary-DC Lease), fencing, the per-engine DR drivers, traffic-steering integration, GitOps wiring, registry replication, the drill and data-integrity harness, audit and evidence export, and policy enforcement (maker-checker, residency, RPO/RTO guardrails) | (consumes the network and storage primitives) | **Provides and builds all of it** | Consumes it through the option surface (Section 12) |
| Database tier: the KubeDB operators, the per-engine DR drivers, and the database-layer replication setup (WAL, binlog, oplog) | Provides the storage and compute it runs on | Provides the operators, the drivers, and the replication orchestration | Provides the database instance spec, schema, and data |
| Application tier: containers, Helm/manifests in Git, and DR-tolerant behavior (write locally, reconcile-on-read, idempotent and retryable clients) | (runs it on the substrate) | Deploys it via GitOps and fails it over | **Provides the application and makes it DR-tolerant** |
| Intent and policy: criticality tier, RPO/RTO targets, sync/async, active/active vs passive, residency, failover governance, drill schedule, retention | (honors residency at the infra layer) | Enforces and realizes the declarations | **Declares all of it** |
| Governance and regulatory: failover approval (maker-checker / two-person), drill attestation, important-business-service mapping, incident reporting to the regulator, the ICT register | (supplies resilience attestations for the bank's register) | Supplies the evidence, audit records, and ICT-register hooks | **Owns the regulatory relationship and the approvals** |

### 13.2 Who operates what (the axis that shifts with the engagement model)

Providing a layer and operating it day to day are different, and the operate split depends on the commercial model. The provide split above is fixed; the operate split has three common shapes:

- **AppsCode-managed (fully managed DBaaS/PaaS):** AppsCode operates the platform control plane and the database and DR lifecycle; the infrastructure provider operates the substrate; the bank operates only its application and its governance. Lowest operational burden on the bank, and the cleanest evidence story.
- **Partner / MSP-operated:** a managed-service partner operates the platform on the bank's behalf, on the infrastructure provider's substrate, using AppsCode's software. AppsCode provides and supports the stack; the MSP operates it; the bank governs.
- **Customer self-managed:** the bank's platform team operates the AppsCode software on the infrastructure provider's substrate. AppsCode provides the software and support; the bank operates everything above the substrate.

In every shape, three things never move: the infrastructure provider owns the substrate and the inter-DC link SLA, AppsCode owns the failover decision logic and the drivers (in the software, whoever runs it), and the bank owns the intent, the approvals, and the regulatory accountability.

### 13.3 Who does what during a failover

The actors in sequence for an unplanned full-DC loss (active/passive), mapping to Section 15:

1. **Infrastructure provider:** a data center fails (power, network); the inter-DC link may drop; the network keeps the witness reachable from DR.
2. **Platform:** the failover controller in the witness observes the primary-DC Lease move (the shared 3-DC quorum has confirmed a full-DC loss, not a partition) and checks the lag guard.
3. **End user (if governance is manual-approval):** the maker-checker / two-person approval authorizes the regional failover; under automatic policy this step is pre-authorized by the configured tier.
4. **Platform:** fences the old primary (the primary-DC Lease expiry self-fences it; the controller revokes credentials), and promotes the DR data tiers as a consistency group, accepting the bounded async loss within the lag guard.
5. **Platform:** flips the traffic steering (BGP withdrawal, k8gb DNS) using the infrastructure provider's network primitives, and commits the active-site change to Git.
6. **Infrastructure provider:** the network reconverges and routes clients to DR.
7. **End user's application:** clients reconnect and retry (the DR-tolerance the user built in); reads tolerate the cross-tier window.
8. **Platform:** surfaces the realized RPO and emits an immutable audit record.
9. **End user:** validates recovery, and reports to the regulator if it is a major incident (the DORA 4-hour / 72-hour / 1-month clocks).

For failback the roles repeat in reverse: the platform orchestrates the planned, zero-RPO switchover, the bank approves the maintenance window, and the infrastructure provider supplies the link for the resync. For a corruption or ransomware event the path is different: the platform restores from the immutable backup (provided by the infrastructure layer) into a clean environment, the bank approves and attests, and the RTO is the slower restore-path number (Section 12).

### 13.4 The boundaries that must not blur

Three handoffs are where DR programs fail in audits, so they are stated explicitly. The **witness must be a genuinely independent failure domain**, which only the infrastructure provider can guarantee, a witness that shares power or network with DC defeats the quorum. The **inter-DC link is the infrastructure provider's SLA**, and its bandwidth and loss behavior set the achievable async RPO and the sync feasibility, so it is contracted, not assumed. The **failover decision, fencing, and promotion are the platform's**, and must not be second-guessed by an operator flipping traffic by hand (which would bypass the fence and risk split-brain); the human role is the maker-checker approval and the post-event attestation, not the mechanism.

---

## 14. Competitive landscape

DR products fall into three architectures, trading RPO and cost against distance:

| | Backup-based DR | Replication / CDP DR | Stretched / synchronous |
|---|---|---|---|
| RPO | minutes to hours (backup interval) | seconds (CDP) to minutes (async) | zero |
| RTO | higher (restore) | low (warm standby) | near zero |
| Distance | any | any | metro only |
| Examples | Velero, Kasten K10, Trilio, Longhorn DR volumes, AWS Backup, GCP Backup-DR | Zerto, Azure Site Recovery, Portworx async, **OpenShift Regional-DR**, AWS DRS, Aurora Global DB | **OpenShift Metro-DR**, Portworx PX-Metro, Pure ActiveCluster, Ceph stretch, Spanner, Cosmos multi-write |

**Kubernetes-native comparators (the most relevant).** **Red Hat OpenShift DR** is the closest: Ramen plus ACM plus ODF/Ceph, hub-orchestrated by CRDs (`DRPolicy`, `DRPlacementControl`), spanning **Regional-DR** (async Ceph RBD mirror, RPO equal to the policy interval, typically five to fifteen minutes) and **Metro-DR** (sync stretched Ceph with a third-site arbiter, RPO zero). It validates this design's choices (Ramen/VRG model, third-site arbiter, consistency groups) and is the system to benchmark against. **Portworx PX-DR** (Pure) is the other mature k8s-native option: PX-Metro sync (RPO zero, RTO under a minute, metro) and async DR (RPO from fifteen minutes, any distance), moving app objects and PV data together via Stork. Backup-based k8s DR (**Velero** as the baseline, **Kasten K10**, **Trilio**) is complementary, not a substitute for replication, because RPO is the backup interval and recovery is operator-run.

**Cloud DR (none Kubernetes-native; they operate at VM, block, object, DNS, or DB layers).** AWS: **Elastic Disaster Recovery** (agent-based continuous async block, RTO minutes), **Route 53 Application Recovery Controller** (readiness and routing only, no data), **Aurora Global Database** (async storage replication, managed switchover at RPO zero), **S3 Cross-Region Replication**. Azure: **Site Recovery** (VM replication and recovery plans, explicitly not automatic, one-hour RTO SLA), **Front Door / Traffic Manager** (global traffic failover), **GRS/RA-GRS** storage, **Cosmos DB** multi-region writes. GCP: **Backup and DR Service**, **Cloud DNS plus global anycast load balancing**, **Spanner** (synchronous Paxos, effectively zero RPO/RTO). VMware: **Live Site Recovery** (ex-SRM) plus vSphere Replication, and **Live Recovery**. **Zerto** (HPE) is the canonical journal-based CDP comparator (near-zero RPO, point-in-time rewind), with **Zerto for Kubernetes** extending CDP to PVs. The recurring gap: the cloud and VM products have no awareness of pods, PVCs, or StatefulSets, which is the argument for a Kubernetes-native design that fails over the application, its volumes, and its routing as one unit.

The differentiation for this platform: a single, regulator-aligned control plane that fails over the **whole application** (stateless tier, database, block, and object), reuses one third-site witness across all tiers, exposes intent-level options (RPO/RTO, residency, maker-checker, drills) rather than storage mechanics, and is assembled from open-source components without a single-vendor array lock-in.

---

## 15. Failover and failback flow, phasing, and open questions

### 15.1 Unplanned failover (full-DC loss), active/passive

1. The witness-hosted controller observes the primary-DC Lease move to DR (the shared quorum, DR plus witness, has confirmed full-DC loss, not a partition).
2. Fence the old primary (credential and route revocation; for the data tiers, the primary-DC Lease expiry self-fences a partitioned writer, per the companion design).
3. Promote the data tiers in DR **as a consistency group**: storage `VolumeReplicationGroup` to primary, database promotion, accepting the bounded async data loss (the worst-tier RPO), gated by the lag guard.
4. Flip traffic: withdraw or de-advertise the DC BGP route and update k8gb to DR; commit the active-site change to Git for the GitOps engine to converge the app's active posture.
5. Reach DR-active. Surface the realized RPO and emit an immutable audit record.

Hard invariant: **promotion strictly precedes traffic admission to DR.** No steering layer (BGP, DNS, or ClusterMesh) may admit writes to DR until the DR data primary is promoted and the old primary is fenced; steps 2 and 3 are what make writes safe, and step 4 is a downstream client-experience optimization, not a safety gate.

### 15.2 Planned failback (DC recovered)

When DC recovers it must come back **fenced and as a standby**: it does not resume as primary and does not re-advertise its anycast VIP as primary on boot (the data tiers return demoted, per the database design's demote-on-reconnect, and the traffic layer keeps de-preferring it until switchover), so a returning DC cannot attract writes or split-brain against the now-active DR. Then reverse-replicate DR to DC, let DC catch up, and run a **planned switchover** (quiesce, drain all three tiers to a common point, flip roles, resume on DC) for a zero-RPO return, discarding any divergent tail on the formerly-failed site, as in the database design.

### 15.3 Phasing

- **Phase 0:** the third-site witness, the Common Cross-DC Failover Service (the 3-DC etcd quorum and OCM control plane exposing the primary-DC Lease), the failover controller, the GitOps engine (Argo CD or Flux) across DC and DR, and per-site Harbor with replication. Anycast (Cilium/MetalLB BGP) and k8gb for the stateless tier.
- **Phase 1:** stateless active/active end to end (case A), the simplest and highest-value.
- **Phase 2:** active/passive block (case C block tier) via `VolumeReplication`/`VolumeReplicationGroup` over Ceph RBD mirror or DRBD, with consistency groups; integrate the database tier from the companion design.
- **Phase 3:** object storage replication (NooBaa / Ceph RGW / MinIO) and the cross-tier inconsistency containment (versioned objects, reconcile-on-read).
- **Phase 4:** active/active metro (case B1) and async multi-master (case B2) for customers who want them; the full regulated-customer option surface (residency, maker-checker, scheduled drills, audit and ICT-register export).

### 15.4 Open questions

1. **Default control plane: resolved to OCM/ACM**, consistent with KubeDB Platform and the failover service's OCM control plane; Karmada is optional for the stateless auto-reschedule tier only. (Confirm the existing hub version when wiring the failover service's external etcd.)
2. **Cross-tier RPO contract:** how to present a single workload RPO when three tiers lag independently, and whether to offer an app-level quiesce hook for tighter coupling on planned events.
3. **Object DR engine default:** MinIO site replication (syncs IAM too) versus Ceph RGW multisite (when the block tier is already Ceph) versus NooBaa (S3 abstraction over heterogeneous backends).
4. **Anycast for stateful front ends:** how aggressively to push L7 session affinity and connection draining so anycast reconvergence does not break regulated transactional sessions.
5. **Witness reuse at scale:** one witness site hosting the controller, the storage arbiter, the shared primary-DC Lease and the failover service's etcd member, and the registry, for many tenants, and its blast radius.

---

## 16. Appendix: references

Block / volume replication:

- DRBD / LINBIT (protocols, DRBD Proxy, dual-primary caveats): https://linbit.com/drbd/ and https://linbit.com/drbd-user-guide/drbd-guide-9_0-en/
- Piraeus operator (LINSTOR CSI on Kubernetes): https://github.com/piraeusdatastore/piraeus-operator
- Ceph RBD mirroring (async, journal vs snapshot): https://docs.ceph.com/en/latest/rbd/rbd-mirroring/
- Rook RBD mirroring: https://rook.io/docs/rook/latest-release/Storage-Configuration/Block-Storage-RBD/rbd-mirroring/
- csi-addons VolumeReplication API: https://github.com/csi-addons/kubernetes-csi-addons/blob/main/docs/volumereplication.md
- Ramen (the engine behind OpenShift DR): https://github.com/RamenDR/ramen
- OpenShift DR (Regional and Metro): https://docs.redhat.com/en/documentation/red_hat_openshift_data_foundation/4.17/html/configuring_openshift_data_foundation_disaster_recovery_for_openshift_workloads/
- Longhorn DR volumes (backup-based): https://longhorn.io/docs/1.10.1/snapshots-and-backups/setup-disaster-recovery-volumes/
- Pure ActiveCluster: https://blog.purestorage.com/products/activecluster-simple-stretch-clustering/
- Portworx PX-DR: https://docs.portworx.com/portworx-enterprise/operations/disaster-recovery/px-metro
- NetApp Trident replication: https://community.netapp.com/t5/Tech-ONTAP-Blogs/Simplifying-disaster-recovery-with-Trident-SnapMirror-volume-replication-for/ba-p/462334

Object storage replication:

- NooBaa / MCG bucket replication (async, uni/bi-directional): https://www.noobaa.io/ and https://docs.redhat.com/en/documentation/red_hat_openshift_data_foundation/4.18/html/managing_hybrid_and_multicloud_resources/multicloud_object_gateway_bucket_replication
- Ceph RGW multisite: https://docs.ceph.com/en/latest/radosgw/multisite/ and sync policy https://docs.ceph.com/en/latest/radosgw/multisite-sync-policy/
- MinIO site replication: https://docs.min.io/aistor/administration/replication/site-replication/ and multi-site active-active https://blog.min.io/minio-multi-site-active-active-replication/
- AWS S3 Cross-Region Replication (RTC 15-min SLA): https://docs.aws.amazon.com/AmazonS3/latest/userguide/replication.html

Traffic, network, orchestration, GitOps, registry:

- MetalLB BGP (ECMP, connection break on reconverge, FRR-K8s): https://metallb.io/concepts/bgp/
- Cilium BGP Control Plane: https://docs.cilium.io/en/stable/network/bgp-control-plane/bgp-control-plane/
- Cilium ClusterMesh service affinity (local-first failover): https://docs.cilium.io/en/stable/network/clustermesh/affinity/
- k8gb (CNCF GSLB, failover strategy, no SPOF): https://www.k8gb.io and https://github.com/k8gb-io/k8gb and https://www.k8gb.io/docs/strategy.html
- Karmada (propagation, cluster failover): https://karmada.io/docs/ and https://karmada.io/docs/userguide/failover/cluster-failover
- Open Cluster Management: https://open-cluster-management.io/docs/
- Argo CD ApplicationSet cluster generator: https://argo-cd.readthedocs.io/en/stable/operator-manual/applicationset/Generators-Cluster/ and HA https://argo-cd.readthedocs.io/en/stable/operator-manual/high_availability/
- Flux (multi-cluster via Kustomize overlays or a remote `kubeConfig`; Flagger for progressive delivery): https://fluxcd.io/flux/ and https://fluxcd.io/flagger/
- Harbor replication: https://goharbor.io/docs/2.12.0/administration/configuring-replication/

Competitors and regulation:

- Red Hat OpenShift DR (Regional async, Metro sync + arbiter): https://docs.redhat.com/en/documentation/red_hat_openshift_data_foundation/4.16/html/configuring_openshift_data_foundation_disaster_recovery_for_openshift_workloads/rdr-solution
- Portworx PX-Metro: https://docs.portworx.com/portworx-enterprise/operations/disaster-recovery/px-metro
- AWS Elastic Disaster Recovery: https://docs.aws.amazon.com/drs/latest/userguide/what-is-drs.html ; Route 53 ARC: https://docs.aws.amazon.com/r53recovery/latest/dg/what-is-route53-recovery.html ; Aurora Global Database: https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/aurora-global-database.html
- Azure Site Recovery: https://learn.microsoft.com/en-us/azure/site-recovery/azure-to-azure-common-questions ; Front Door routing: https://learn.microsoft.com/en-us/azure/frontdoor/routing-methods
- GCP Spanner (sync, external consistency): https://docs.cloud.google.com/spanner/docs/true-time-external-consistency ; Backup and DR: https://docs.cloud.google.com/backup-disaster-recovery/docs/concepts/backup-dr
- VMware Live Site Recovery: https://core.vmware.com/resource/site-recovery-manager-technical-overview ; Zerto: https://www.hpe.com/us/en/zerto-software.html
- BCBS 239: https://www.bis.org/publ/bcbs239.pdf
- EU DORA Regulation (EU) 2022/2554: https://eur-lex.europa.eu/eli/reg/2022/2554/oj/eng
- RBI Master Direction on IT Governance (RTO/RPO per system): https://www.rbi.org.in/Scripts/BS_ViewMasDirections.aspx?id=12562 ; RBI payment-data localisation: https://www.rbi.org.in/commonman/english/scripts/FAQs.aspx?Id=2995
- FFIEC Business Continuity Management booklet: https://ithandbook.ffiec.gov/it-booklets/business-continuity-management
- BIS Principles for Operational Resilience: https://www.bis.org/bcbs/publ/d516.htm ; UK PRA SS1/21: https://www.bankofengland.co.uk/-/media/boe/files/prudential-regulation/publication/2021/building-operational-resilience-impact-tolerances-for-important-business-services.pdf

Note: a few regulatory specifics (the exact RBI "near-zero RPO" and "identical DR" wording and drill frequency) should be quoted from the official RBI PDFs before publishing; DORA articles should be cited to EUR-Lex. KubeDB and version-pinned vendor links should be re-pinned to the release the implementation targets.



