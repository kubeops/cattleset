# DEV.md

The knowledge a Go developer needs to work on `kubeops.dev/cattleset`.

## Build flow

All Make targets run inside the `ghcr.io/appscode/golang-dev` Docker image, so Docker must be running. The host needs only `make` and Docker; the toolchain, codegen binaries, and linters live in the image.

- `make build` build the host binary.
- `make gen` regenerate the clientset and the CRD manifests. Run it after any change to `apis/apps/v1/*_types.go`.
- `make manifests` CRDs only; `make clientset` the `client/` tree only.
- `make fmt`, `make lint`, `make test`, `make verify` standard.
- `make container` build the three image variants.

For quick iteration a local Go toolchain works for `go build ./...` and `go test ./...`; the Docker flow is what CI runs and what `make verify` checks.

## The codegen pipeline

`apis/apps/v1/*_types.go` is the single source of truth. `make gen` runs:

- `controller-gen` to produce `zz_generated.deepcopy.go` and the CRD YAML in `crds/`.
- the AppsCode gengo image to produce the typed clientset, listers, and informers under `client/`.

Never hand-edit `zz_generated.*.go`, anything under `client/`, or the files in `crds/`; change the types and re-run `make gen`. `make verify` fails if the generated tree drifts from the committed one.

## Vendoring policy

The `vendor/` directory is checked in. `go mod tidy && go mod vendor` must leave the tree clean, enforced by `make verify` (verify-modules). `kubeops.dev/petset` is a dependency (its API types and clientset) and is never modified from this repo. It is pinned as a released version in `go.mod`, with no `replace`. While the changes this design needs (`FailoverGroupMember.handoffTo`, an optional `clusterSpreadConstraint.slice`) are not yet in a release, the pin is a pseudo-version of the petset branch that carries them; move it to the release tag once that is tagged. To work against a local petset, add a `replace` for your own checkout temporarily and do not commit it.

## Concepts in play

- **The DeploymentSpec mirror.** The first eight fields of `CattleSetSpec` are `apps/v1` `DeploymentSpec` field for field. `apis/apps/v1/deployment_pin_test.go` pins this with reflection against the vendored `k8s.io/api`, so an upstream field change fails the build rather than silently diverging. `FromDeployment` is a mechanical copy.
- **Server-side apply and field ownership.** The controller is the single writer of every rendered per-site object, using one field manager. For a gated Service the ManifestWork entry uses `ServerSideApply` with `ignoreFields` on `spec.selector` so the spoke gate agent (not the work agent) owns the admission flip after creation.
- **OCM ManifestWork and feedback rules.** In distributed mode each Member cluster gets two ManifestWorks: `cattleset-<ns>-<name>` carries that site's Deployment, Service, PDB and optional traffic objects, and `...-deps` carries the namespace and the `syncResources` copies. FeedbackRules project the Deployment's `readyReplicas`, `updatedReplicas` and `availableReplicas` back into `status.sites`, and, for a gated Service, the gate agent's `admission` and `admission-observed-at` annotations (JSONPath on an annotation key, which is a lab check).
- **The coordination plane.** dr-controlplane publishes the primary DC Leases (`primary-dc-<group>`) and health Leases (`dc-health-<dc>`) in `dc-failover`, and each spoke's agent projects a marker ConfigMap per Lease. The hub controller reads the Leases (read only, uncached) and watches them for holder and annotation changes only. The gate agent reads the markers. Neither writes a Lease. Unreadable never means "no holder": capacity holds, and admission closes.
- **Gateway API.** Traffic rendering uses `gwv1.ParentReference` for HTTPRoute; the API surface kept in vendor is deliberately small.

## Observability

Metrics register with the controller-runtime registry (`pkg/controller/cattleset/metrics.go` and `pkg/controller/gate/metrics.go`):

- `cattleset_posture_flips_total{namespace,name}` primary-DC flips realized per herd.
- `cattleset_observe_to_converge_seconds{namespace,name}` Lease change to PostureConverged.
- `cattleset_site_ready_replicas` / `cattleset_site_desired_replicas{namespace,name,cluster}` per-site gauges.
- `cattleset_gate_flips_total{namespace,service,result}` gate admits and revokes.

Events: `PostureFlip` on the controller when the primary DC moves (the spec, and therefore `metadata.generation`, is unchanged); `SwitchoverRejected`, `SwitchoverAborted`, `SwitchoverTimedOut` and `SwitchoverComplete` for a planned switchover; `SitePruned` and `HubLocalRetired`; and `SiteAdmitted` / `SiteRevoked` on the gated Service from the spoke gate agent, with the reason.

## Packaging

The Helm chart is `charts/cattleset` in `kubeops.dev/installer`, modeled on `charts/petset` file for file. The reference manifests in `deploy/` show the two roles (hub controller, per-spoke gate agent) and the RBAC each needs; keep them and the chart in step with the flags in `pkg/cmds/server`. Its chart-values Go types are `apis/installer/v1alpha1/cattleset_types.go`, and `values.schema.json` plus `values.openapiv3_schema.yaml` are generated by the installer repo's `make gen` (never hand-written). The chart does not ship the `PlacementPolicy` CRD; petset's chart owns it, so install `charts/petset` (or otherwise ensure the CRD is present) first. The chart installs the hub controller; the gate agent is a separate per-spoke install (`--hub-controller=false --gate-agent --dc-name=<spoke>`), because it must run on every Member and holds no hub credential.
