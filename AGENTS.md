# AGENTS.md

This file provides guidance to coding agents (e.g. Claude Code, claude.ai/code) when working with code in this repository.

## Repository purpose

Go module `kubeops.dev/cattleset`. CattleSet is to Deployment what PetSet is to StatefulSet: the same stateless workload contract, made data-center aware. It defines one namespaced CRD under `apps.k8s.appscode.com`:

- `CattleSet` runs one stateless application across the data centers named by a `PlacementPolicy`, as one object with one spec. With `distributed: false` (the default) it manages one local `apps/v1` Deployment and behaves exactly like a Deployment. With `distributed: true` the controller renders one Deployment per Member data center (via OCM `ManifestWork`), follows the primary-DC Lease, and realizes the declared posture (ActiveActive or ActivePassive).

The produced binary is `cattleset`.

`PlacementPolicy` (the placement, DC roles, KubeSlice, and failover trigger) is owned by `kubeops.dev/petset`. This repo imports those API types and the generated clientset from `kubeops.dev/petset`; it never redeclares them and never installs a second copy of the CRD. CattleSet code imports only petset's published API types and clientset, never its controller internals.

The design lives in `DESIGN.md` (seeded from `cattleset-design.md`).

## Architecture

- `cmd/cattleset/` entry point.
- `pkg/cmds/` Cobra root plus run; `pkg/cmds/server/` builds the controller-runtime manager.
- `apis/apps/v1/` Kubebuilder API types (`CattleSet`), `groupversion_info.go`, `install/`, `fuzzer/`, generated `zz_generated.*.go`.
- `client/` generated typed clientset.
- `crds/` generated CRD YAML (`apps.k8s.appscode.com_cattlesets.yaml`) plus `lib.go`.
- `pkg/controller/cattleset/` the reconciler, renderer, posture engine, and ManifestWork control.
- `pkg/webhooks/apps/v1/` admission webhooks.
- `Dockerfile.in` (PROD, distroless), `Dockerfile.dbg` (debian), `Dockerfile.ubi` (Red Hat certified) three image variants, one binary.
- `hack/`, `Makefile` the AppsCode build harness.
- `vendor/` checked-in deps.

## Common commands

All Make targets run inside `ghcr.io/appscode/golang-dev`. Docker must be running.

- `make gen` regenerate clientset plus manifests. Run after changes to `apis/apps/v1/*_types.go`.
- `make manifests` regenerate CRDs only.
- `make clientset` regenerate `client/` only.
- `make fmt`, `make lint`, `make unit-tests` / `make test` standard.
- `make verify` verify-gen plus verify-modules; `go mod tidy && go mod vendor` must leave the tree clean.
- `make container` build PROD, DBG, and UBI images.
- `make add-license` / `make check-license` manage license headers.

Run a single Go test (requires a local Go toolchain):

```
go test ./pkg/controller/cattleset/... -run TestName -v
```

## Hard rules (from the design)

- **The spec core mirrors `apps/v1` `DeploymentSpec` field for field** (names, types, json tags, defaults). A CI round-trip test (`apis/apps/v1/deployment_pin_test.go`) pins it against the vendored `k8s.io/api`; an upstream field change fails the build. `FromDeployment` converts mechanically.
- **The spec is posture-invariant.** A failover never edits the CattleSet spec, and therefore never touches Git. The controller is the single writer of every rendered per-site object.
- **Do not fork the Deployment controller.** CattleSet renders and owns plain `apps/v1` Deployments; stock Kubernetes reconciles their pods.
- **The write-path gate acts at traffic admission, never pod readiness.** No pod `readinessGate` anywhere in the tree.
- **Admission is spoke-local.** A hub outage never blocks a legitimate admission nor forces a wrong one.
- **Pods run only in Member DCs.** A PlacementPolicy `Witness` rule is treated exactly as `Arbiter`.
- **`gates` requires `topology.mode: ActivePassive`**, and every gated data tier must follow the same `failoverPolicy.trigger` as the herd.
- **Fail closed on the Lease:** cannot-read-or-renew means not-primary; use a consistent read or watch for a posture decision. Steering is never fencing.

## Conventions

- Module path is `kubeops.dev/cattleset` (vanity URL). Imports must use that.
- License: Apache-2.0 (`LICENSE`); new files need the standard AppsCode header (`make add-license`).
- Sign off commits (`git commit -s`); contributions follow the DCO.
- Vendor directory is checked in; `go mod tidy && go mod vendor` must leave the tree clean (enforced by `verify-modules`).
- Do not hand-edit `zz_generated.*.go`, anything under `client/`, or `crds/`; change `apis/apps/v1/*_types.go` and re-run `make gen`.
- API group is `apps.k8s.appscode.com` (shared with PetSet and PlacementPolicy).
- Three Dockerfiles, one binary; keep `Dockerfile.in`, `Dockerfile.dbg`, and `Dockerfile.ubi` in sync.
- No em-dashes anywhere (docs, comments, commit messages). Commas, periods, colons, parentheses.
- Repo structure mirrors `kubeops.dev/petset` one for one; `hack/structure-parity.sh` diffs the Makefile targets and workflow jobs against petset in CI.
