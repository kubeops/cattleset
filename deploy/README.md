# Reference manifests

These are the plain-manifest form of the two CattleSet roles. The Helm chart (`charts/cattleset` in `kubeops.dev/installer`) is the supported install and is generated from the same flags and RBAC; use these to read what each role needs, or to run CattleSet without the chart.

- `hub.yaml`: the hub controller and its admission webhook. Two replicas, leader election on, no gate agent. Install once, on the OCM hub.
- `gate-agent.yaml`: the spoke gate agent. Install on **every Member spoke**, with `--dc-name` set to that spoke's managed cluster name. In production the OCM add-on that ships dr-controlplane's agent can deliver it by ManifestWork instead.

Prerequisites on the hub: OCM (the `ManifestWork` CRD), petset's CRDs (`PlacementPolicy`, `FailoverGroup`), and dr-controlplane. A webhook needs a serving certificate in `cattleset-webhook-tls` and the `MutatingWebhookConfiguration` and `ValidatingWebhookConfiguration` with its CA bundle (cert-manager or the installer chart does this; it is not repeated here).

The hub controller needs **no** write access to Leases. The coordination plane is read through `--coord-kubeconfig` (a Secret mounted at `/coord/kubeconfig`), or the hub's own API server when that is empty.
