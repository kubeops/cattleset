# CattleSet

CattleSet is to Deployment what PetSet is to StatefulSet: the same stateless workload contract, made data-center aware. A CattleSet runs one stateless application across the data centers named by a `PlacementPolicy`, as one object with one spec, and follows the failover of the databases the application writes to.

Pets are named and irreplaceable; cattle are interchangeable and managed as a herd. A PetSet gives each pod a stable identity and storage; a CattleSet manages an interchangeable herd across data centers.

## The two modes

- **Non-distributed (`distributed: false`, the default).** The CattleSet manages one local `apps/v1` Deployment and behaves exactly like a Deployment: same rollouts, same revision history, HPA passthrough through the scale subresource. The spec core mirrors `apps/v1` `DeploymentSpec` field for field, so a Deployment spec pastes in unchanged.

- **Distributed (`distributed: true`).** The controller renders one Deployment per Member data center (through OCM `ManifestWork`), and the application takes part in KubeDB's DC/DR ordering as its own `FailoverGroup` that `dependsOn` the databases it writes to. Traffic is admitted to a site only while those databases are active and writable in **that same cluster**; a spoke-local gate agent decides that from local objects and fails closed. A failover changes no field of the spec, so it touches no Git.

## Quick start

Non-distributed, a Deployment in all but name:

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

### An application over a DC-DR Postgres

The database (`pg-docs`, in namespace `demo`) already has its own FailoverGroup and PlacementPolicy. The application gets its own group that depends on it, and lives in its **own namespace**: the database's namespace is on the KubeSlice, which would inject its sidecar into anything deployed there. The application reaches the database through the database's Service in its own cluster, `pg-docs.demo.svc.cluster.local`.

```yaml
apiVersion: apps.k8s.appscode.com/v1
kind: FailoverGroup
metadata: { name: orders-app }
spec:
  dependsOn: [pg-docs]
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
          env:
            - { name: DB_HOST, value: pg-docs.demo.svc.cluster.local }
  podPlacementPolicy: { name: orders-app-geo }
  topology:
    standbyReplicas: 6          # a hot standby; the mode defaults to Auto
  syncResources:                # copied to every Member, along with the namespace
    - { kind: ConfigMap, name: orders-api-config }
    - { kind: Secret, name: orders-db-credentials }
  traffic:
    serviceName: orders-api
    ports:
      - { name: http, port: 80, targetPort: 8080 }
```

After it reconciles, `status.derivedMode` is `ActivePassive`, `status.dependencies[0].camp` is `SingleWriter`, the Service in the standby DC is unadmitted (`admissionReason: NotActiveHere`), and the Service in the active DC is admitted only once its Postgres pod is `role=primary`.

More shapes (a leaderless database such as Cassandra, a stateless herd with no database) and every rule are in [`DESIGN.md`](DESIGN.md). Enabling DR later is a spec change (`distributed: true`, a `podPlacementPolicy`), not a new object: the controller adopts the local Deployment as the local site's rendered output and seeds the other sites.

## Install

Two roles, installed separately:

- the **hub controller** (and its webhook), once, on the OCM hub, with leader election and two replicas;
- the **gate agent**, on **every Member spoke**, with `--dc-name` set to that spoke's managed cluster name.

The Helm chart is `charts/cattleset` in `kubeops.dev/installer`; it needs `charts/petset` first (petset owns the `PlacementPolicy` and `FailoverGroup` CRDs) and dr-controlplane. Reference manifests for both roles, with the exact RBAC each needs, are in [`deploy/`](deploy/). The hub controller needs no write access to Leases; the coordination plane is read through `--coord-kubeconfig`.

Planned switchover of an application group needs petset's `FailoverGroupMember.handoffTo` and the matching dr-controlplane release. Unplanned failover, admission and capacity do not.

## Upgrading from the earlier design

- `spec.gates` is removed and rejected. Declare the dependency on the FailoverGroup instead: put the application in its own group and `dependsOn` the database's. Until the FailoverGroup exists the Service stays closed, never open.
- `spec.traffic.export` is removed and rejected: a stateless CattleSet does not use KubeSlice.
- `spec.traffic.ports` is required when `traffic` is set. The Service port used to be a hard-coded 80 to 8080; list it explicitly.
- `topology.mode` defaults to `Auto` and `topology` may be omitted. An existing explicit mode keeps its meaning.
- `status.currentPrimaryDC` and `sites[].gated` are replaced by `status.servingDCs`, `status.failoverGroup.leaseHolder` and `sites[].admitted`.
- The Lease is `primary-dc-<group>` in `dc-failover`, derived from `failoverGroupRef`. `failoverPolicy.trigger` no longer exists in petset.
- A CattleSet gains a finalizer that removes its ManifestWorks. The old gate agent and the new one must not run together (both write the Service selector); roll the hub controller and the agents together.

See `DESIGN.md` for the full design, `DEV.md` for how to build and hack on the repo, and `TEST.md` for how to test it.
