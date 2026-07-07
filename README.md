# CattleSet

CattleSet is to Deployment what PetSet is to StatefulSet: the same stateless workload contract, made data-center aware. A CattleSet runs one stateless application across the data centers named by a `PlacementPolicy`, as one object with one spec.

Pets are named and irreplaceable; cattle are interchangeable and managed as a herd. A PetSet gives each pod a stable identity and storage; a CattleSet manages an interchangeable herd across data centers.

## The two modes

- **Non-distributed (`distributed: false`, the default).** The CattleSet manages one local `apps/v1` Deployment and behaves exactly like a Deployment: same rollouts, same revision history, HPA passthrough through the scale subresource. The spec core mirrors `apps/v1` `DeploymentSpec` field for field, so a Deployment spec pastes in unchanged. Teams adopt one workload API everywhere.

- **Distributed (`distributed: true`).** The controller renders one Deployment per Member data center (via OCM `ManifestWork`), watches the shared primary-DC Lease from the Common Cross-DC Failover Service, and continuously realizes the declared posture:
  - **ActiveActive:** every Member serves, anycast or round-robin GSLB in front; a lost Member's share is absorbed by survivors.
  - **ActivePassive:** the Lease holder serves, the other Members hold a warm standby.

The central property is that **the spec is posture-invariant**: a CattleSet declares both postures up front and the controller flips between them by following the Lease. Failover requires no spec edit and no Git commit.

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

Enabling DR later is a spec change (`distributed: true`, a `podPlacementPolicy`, a `topology`), not a new object: the controller adopts the local Deployment as the local site's rendered output and seeds the other sites.

See `DESIGN.md` for the full design, `DEV.md` for how to build and hack on the repo, and `TEST.md` for how to test it.

## Install

The Helm chart is `charts/cattleset` in `kubeops.dev/installer`. It requires `charts/petset` to be present (petset owns and installs the `PlacementPolicy` CRD).
