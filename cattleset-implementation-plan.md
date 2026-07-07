# CattleSet Implementation Plan (for Claude Code)

**Scope:** implement the `CattleSet` CRD and controllers per `cattleset-design.md`.
**Audience:** Claude Code, executing against a **new repo `kubeops.dev/cattleset`** (GitHub: https://github.com/kubeops/cattleset; the CRD and controllers), with `kubeops.dev/petset` as a library dependency (the `PlacementPolicy` types and the conventions to copy), `kubeops.dev/installer` (the Helm chart home: `charts/cattleset`, modeled on `charts/petset`), the KubeDB Platform charts (platform integration), and the Common Cross-DC Failover Service deliverables (consumed, not built here).
**Companions:** `cattleset-design.md` (the design this implements), `kubedb-dc-failover-service-implementation-plan.md` (F6 client library and the Section 5 consumer contract), `kubedb-dc-dr-implementation-plan.md` (Section 4 three-cluster harness).

---

## 0. How to use this plan

Feed Section 1 (repo orientation) first, then the Foundation tasks (Section 2, C0 to C10) in the Section 4 PR order. Conventions per task: Context, Files, Implement, Tests, Done when.

**Hard rules from the design that must hold in every task:**

- **The spec core mirrors `apps/v1` `DeploymentSpec` field for field** (names, types, json tags, defaults). A CI round-trip test pins it against the vendored `k8s.io/api`; an upstream field change fails the build. `FromDeployment` converts mechanically.
- **The spec is posture-invariant.** A failover never edits the CattleSet spec, and therefore never touches Git. The controller is the single writer of every rendered per-site object; users and GitOps write only the CattleSet and PlacementPolicy.
- **Do not fork the Deployment controller.** CattleSet renders and owns plain `apps/v1` Deployments; stock Kubernetes reconciles their pods. (PetSet had to fork StatefulSet; CattleSet must not repeat that.)
- **The write-path gate acts at traffic admission, never pod readiness.** The rendered Service of a gated herd carries the admission selector term; the spoke gate agent flips it. No pod `readinessGate` anywhere: gated-not-ready pods wedge rollouts and PDBs.
- **Admission is spoke-local.** A hub outage never blocks a legitimate admission nor forces a wrong one.
- **Pods run only in Member DCs.** A PlacementPolicy `Witness` rule is treated exactly as `Arbiter` (a herd holds no data).
- **`gates` requires `topology.mode: ActivePassive`**, and every gated data tier must follow the same `failoverPolicy.trigger` as the herd.
- **Fail closed on the Lease:** cannot-read-or-renew means not-primary; use a consistent read or watch, never a cached list, for a posture decision. Steering (Service, HTTPRoute, GSLB, BGP) is never fencing.
- **Repo conventions and structure parity:** `kubeops.dev/cattleset` mirrors the petset repo's project skeleton one for one, only names differ: the same GitHub workflows, Makefile target set, `hack/` scripts, lint config, and top-level layout. It adopts the AGENTS.md conventions wholesale: `make gen` after type changes, never hand-edit `zz_generated.*.go` / `client/` / `crds/`, vendor checked in and clean (`make verify`), Apache-2.0 headers (`make add-license`), DCO sign-off. CattleSet code never imports petset controller internals, only its published API types and clientset.
- **No em-dashes anywhere** (docs, comments, commit messages). Commas, periods, colons, parentheses.

---

## 1. Repo orientation (do this first, output a repo map)

The implementation lives in a **new repo, `kubeops.dev/cattleset`** (GitHub `https://github.com/kubeops/cattleset`, module path `kubeops.dev/cattleset` as the vanity import, binary `cattleset`). `kubeops.dev/petset` is read as the template and imported as a dependency, never modified. Read in `kubeops.dev/petset`:

- `AGENTS.md` (conventions to adopt) and `Makefile` (the variable block `PRODUCT_OWNER_NAME` / `PRODUCT_NAME` / `GO_PKG` / `REPO` / `BIN`, `CRD_OPTIONS`, `CODE_GENERATOR_IMAGE`, `API_GROUPS ?= apps:v1`, and the full target set: `gen`, `manifests`, `clientset`, `verify`, `fmt`, `lint`, `test`, `e2e-tests`, `container`, `push`, `release`, `install`, `add-license`).
- `.github/workflows/` (`ci.yml`, `release.yml`, `release-tracker.yml`), `.golangci.yml`, `.gitignore`, `.config/`, and `hack/` (`boilerplate.go.txt`, `build.sh`, `fmt.sh`, `test.sh`, `e2e.sh`, `coverage.sh`, `license/`, `scripts/update-release-tracker.sh`, `samples/`): the skeleton to reproduce verbatim with only name substitutions.
- `apis/apps/v1/`: `petset_types.go` (Deployment-adjacent conventions, scale subresource markers), `placementpolicy_types.go` (`ClusterSpreadConstraint`, `DistributionRule.Role`, `FailoverPolicy`, `FailoverTrigger`, already merged; imported by cattleset, never redeclared), `groupversion_info.go`, `install/`, `fuzzer/`.
- `pkg/controller/petset/pet_manifest_work_control.go`: the ManifestWork create/get/update/delete and `FeedbackRule` projection pattern to port (CattleSet projects Deployment status instead of pod status; port the pattern into the new repo, do not import the package).
- `pkg/controller/placement.go`: how `PlacementPolicy` is resolved and applied.
- `pkg/webhooks/apps/v1/`: the webhook wiring shape to reproduce.
- `cmd/petset/`, `pkg/cmds/`: the binary and Cobra layout to reproduce.

Read in `kubeops.dev/installer` (the chart home):

- `charts/petset/`: the chart to model `charts/cattleset` on: `Chart.yaml`, `OWNERS`, `README.md`, `doc.yaml`, `crds/` (vendored CRD yamls), `templates/` (deployment, RBAC, service, secret, cert-manager wiring, mutating and validating webhooks, PDB, network policy, servicemonitor, `ocm/addon`, `ocm/work-agent`, and the seeded `placementpolicies/`).
- `apis/installer/v1alpha1/petset_types.go`: the chart-values schema convention (Go types that generate `values.schema.json` and `values.openapiv3_schema.yaml` via the installer repo's `make gen`).

Deliverable: a repo map plus answers to seven questions. (1) **API group: decided, `apps.k8s.appscode.com/v1`**, the same group as `PetSet` and `PlacementPolicy`, even though the CRD ships from a different repo (a CRD's group is just its name; nothing requires one repo per group). This is fixed and not revisited. (2) **CRD ownership:** `PlacementPolicy`'s CRD is owned and installed by petset; the cattleset chart must require it (chart dependency or install-time check), never ship a second copy. (3) Is `sigs.k8s.io/gateway-api` needed in the new repo's vendor for `HTTPRoute` rendering (keep the API surface to `ParentReference`)? (4) Where does the failover service client library (its plan's F6) live, and its module path? (5) Where does the spoke-side ManifestWork applier run, so the gate agent (C6) can ride along or ship as the cattleset spoke agent from this repo's chart? (6) For the distribution flip (C9): a non-distributed CattleSet lives in a workload cluster while a distributed one is reconciled from the hub, so confirm whether the CR moves to the hub on the flip or the hub is also a Member in the target topologies, and specify the adoption handshake accordingly. (7) The canonical duck-typed path of KubeDB Database DR status (`status.disasterRecovery`), read via unstructured so cattleset takes no kubedb module dependency.

---

## 2. Foundation tasks

### C0. Repo bootstrap

**Context.** A new repo must carry the full AppsCode operator harness before any feature work.

**Files.** The `github.com/kubeops/cattleset` repo root, matching petset's layout file for file: `go.mod` (module `kubeops.dev/cattleset`, the vanity import path), `Makefile`, `.github/workflows/ci.yml` / `release.yml` / `release-tracker.yml`, `.golangci.yml`, `.gitignore`, `.config/`, `hack/` (`boilerplate.go.txt`, `build.sh`, `fmt.sh`, `test.sh`, `e2e.sh`, `coverage.sh`, `license/`, `scripts/update-release-tracker.sh`, `samples/`), `Dockerfile.in` / `Dockerfile.dbg` / `Dockerfile.ubi`, `LICENSE`, `README.md`, `DESIGN.md`, `TEST.md`, `DEV.md`, `AGENTS.md`, `CLAUDE.md` (`@AGENTS.md`), `cmd/cattleset/`, `pkg/cmds/` (Cobra root and run), empty `apis/apps/v1/` with `groupversion_info.go`, `client/`, `crds/lib.go`. The Helm chart does not live here: it is `charts/cattleset` in `kubeops.dev/installer` (C10).

**Implement.** Copy the petset skeleton verbatim and change only names: Makefile variables (`PRODUCT_NAME := cattleset`, `BIN := cattleset`, `GO_PKG := kubeops.dev`, `API_GROUPS ?= apps:v1` unchanged), workflow product references, image repos, and the vanity import path. No structural edits: the same targets, the same jobs, the same scripts, so petset muscle memory and release tooling transfer unchanged. Vendor `kubeops.dev/petset` (API types and clientset only) and the OCM work API; wire `make gen`, `make manifests`, `make clientset`, `make verify`, `make e2e-tests` to work on an empty API. Seed the documentation set as first-class files, each with a defined owner task keeping it current:
- `README.md`: repo intro and the user guide (what a CattleSet is, the two modes, quick start; grows with C3 and C10).
- `DESIGN.md`: the design of the software, seeded from `cattleset-design.md` and updated whenever an implementation decision lands (the repo-local source of truth once code exists).
- `TEST.md`: how to test the repo (unit tests, envtest, the three-cluster e2e harness and its prerequisites, running one suite); every task that adds a suite updates it.
- `DEV.md`: the knowledge a Go developer needs to work on this repo: the Docker-based make flow, the codegen pipeline and when to run `make gen`, the vendoring policy, and the concepts in play (server-side apply and field ownership, OCM ManifestWork and feedback rules, Lease-based leader election, Gateway API, the DeploymentSpec mirror contract).
- `AGENTS.md`: bootstrap by running `/init` with a coding agent on the fresh skeleton, then curate: the petset conventions plus the Section 0 hard rules.
- `CLAUDE.md`: exactly `@AGENTS.md`.

**Tests.** CI green on the empty skeleton: build, lint, `make verify` idempotent, license check. A structure-parity check (a script in `hack/`) diffs the Makefile target list and the workflow job list against petset's, allowing only the name substitutions; it runs in CI so the skeletons cannot silently drift.

**Done when.** A trivial `make gen && make verify && make container` passes from a fresh clone, and all six documentation files exist with their seed content.

### C1. API types, codegen, and the DeploymentSpec pin

**Context.** `cattleset-design.md` Section 4: the spec core is `DeploymentSpec` field for field, then the optional DC/DR extension block; status mirrors the `DeploymentStatus` aggregates plus `sites`, `currentPrimaryDC`, `selector`, and conditions.

**Files.** In `kubeops.dev/cattleset`: `apis/apps/v1/cattleset_types.go`, `apis/apps/v1/cattleset_helpers.go` (`FromDeployment`, posture helpers), registration in `groupversion_info.go` / `install/` / `fuzzer/`, generated `client/`, `crds/apps.k8s.appscode.com_cattlesets.yaml` (the group is `apps.k8s.appscode.com`, fixed to match PetSet, orientation (1)). `PlacementPolicy` types come from the vendored `kubeops.dev/petset`, never redeclared.

**Implement.**
- The types exactly as designed: `CattleSet`, `CattleSetSpec` (Deployment core then extension), `CattleTopology`, `CattleShare`, `SitePatch`, `CattleRolloutOrder`, `CattleTraffic`, `CattleGSLB`, `CattleHTTPRoute`, `CattleGates`, `CattleSetStatus`, `CattleSiteStatus`, `CattlePosture`. Scale subresource marker with `specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector`. Printer columns: MODE, PRIMARY, DESIRED, READY, SITES, AGE.
- The pin: a unit test that walks `apps/v1.DeploymentSpec` with reflection and asserts the first eight `CattleSetSpec` fields match name, json tag, and type one for one (and the same for the status aggregates against `DeploymentStatus`). A vendor bump that changes upstream fails this test.
- `FromDeployment(d *appsv1.Deployment) *CattleSet`: copies the spec core verbatim, carries labels and annotations, sets no extension fields.
- Vendor `sigs.k8s.io/gateway-api` per orientation (3); import only the `ParentReference` type surface. `make gen`, `make verify` clean.

**Tests.** The reflection pin (spec and status), `FromDeployment` round-trip (Deployment in, spec-identical core out), fuzzer round-trip for the new types, CRD schema golden test (scale subresource paths present).

**Done when.** `make gen && make verify` is idempotent and clean; the pin test fails if a mirrored field is renamed locally (prove it once by mutation, then revert).

### C2. Admission webhook (validation and defaulting)

**Context.** Design Section 8, all rules.

**Files.** `pkg/webhooks/apps/v1/cattleset.go` in `kubeops.dev/cattleset`, reproducing the petset webhook wiring shape.

**Implement.** Validations: stateless template (reject PVC volume sources); `distributed: true` requires `podPlacementPolicy` (with a `clusterSpreadConstraint`) and `topology`; `distributed: false` forbids `topology`, `sitePatches`, `rolloutOrder`, `gates`, `traffic.export`, `traffic.gslb` (a referenced PlacementPolicy may carry only local constraints); `distribution` and `sitePatches` name only clusters in the `distributionRules`, `distribution` only Members; `sitePatches` cannot touch `replicas`, `selector`, selector labels, or the admission gate; `gates` requires ActivePassive and same-trigger data tiers (resolve via unstructured, orientation (7)); `selector` and `topology.mode` immutable; `distributed` true to false rejected. Defaults: upstream trio (`replicas` 1, `revisionHistoryLimit` 10, `progressDeadlineSeconds` 600) always; `standbyReplicas` = `replicas`, `surgeOnFailover` = `100%`, and `rolloutOrder` by mode only when `distributed: true` (defaulting a forbidden extension field on a non-distributed object would trip the webhook's own validation).

**Tests.** Table-driven accept/reject per rule; defaulting golden objects; immutability on update ops.

**Done when.** Every Section 8 rule has a test that fails without the webhook and passes with it.

### C3. Non-distributed reconciler (Deployment parity)

**Context.** Design Section 5.1: `distributed: false` owns exactly one local Deployment (plus Service and HTTPRoute if set) and behaves like a Deployment. This mode ships first and alone is useful.

**Files.** `pkg/controller/cattleset/` (new package): reconciler, renderer, owner-ref and server-side-apply helpers.

**Implement.** Render the Deployment by copying the spec core verbatim (the mirror makes this a literal field copy); set controller owner ref and the CattleSet identity labels; server-side apply with a single field manager (the single-writer rule); render Service and HTTPRoute when set (admitted, no gate in this mode); surface the owned Deployment's status into the CattleSet status (aggregates, `status.selector` serialized from the label selector); scale subresource passthrough; `paused` passthrough.

**Tests.** Parity suite: for a corpus of Deployment specs (including probes, strategies, surge/maxUnavailable variants), assert the rendered Deployment spec is byte-identical to the input core; rollout, rollback via `revisionHistoryLimit`, pause, scale via the subresource, HPA binds through `status.selector` (envtest with the HPA scale client); deleting the CattleSet garbage-collects the Deployment.

**Done when.** A user can `FromDeployment` any existing Deployment, apply it, and observe identical behavior, pods not restarted when adopting an existing same-spec Deployment by name (server-side apply produces no diff).

### C4. Posture engine and distributed rendering

**Context.** Design Sections 5.2, 5.3: per-site desired counts from (mode, roles, Lease holder, per-DC health); one ManifestWork per Member cluster carrying that site's Deployment, Service, PDB, and optional HTTPRoute, ServiceExport, and Gslb; Deployment status projected back by FeedbackRules.

**Files.** `pkg/controller/cattleset/posture.go` (a pure function, unit-testable without a cluster), `pkg/controller/cattleset/manifest_work_control.go` (the pattern ported from petset's `pet_manifest_work_control.go`, but one ManifestWork per workload per cluster, not per pod, and living in this repo).

**Implement.**
- `ComputePosture(mode, rules, holder, health) map[cluster]SiteDesired`: ActiveActive splits `replicas` by `distribution` weights (even default, largest-remainder rounding so the total is exact); ActivePassive gives the holder `replicas` and other Members `standbyReplicas`; Arbiter and Witness always zero; a stale Member (health Lease) triggers the ActiveActive surge.
- Render per site: copy the spec core, apply the site's `sitePatches` (strategic merge, after validation), set the site replica count, render the Service and PDB (for a gated herd the Service renders **unadmitted**, the admission selector term present, and its ManifestWork entry sets `updateStrategy: ServerSideApply` with `ignoreFields` on `spec.selector` so the work agent never reverts the gate agent's flip; C6 only ever flips that term), pack into the ManifestWork with FeedbackRules for `readyReplicas`, `updatedReplicas`, `availableReplicas`, `observedGeneration`.
- Aggregate feedback into `status.sites[]` and the DeploymentStatus-mirror totals; set `PostureConverged`, `Available`, `Progressing` conditions; `status.currentPrimaryDC`.
- A dead cluster's ManifestWork is left desired (OCM applies it on return), which is what makes a returning site come back in standby posture by construction.

**Tests.** Posture table tests (splits, rounding, surge, holder flips, zero for Arbiter and Witness); render golden ManifestWorks including sitePatches; envtest with faked ManifestWork status feedback driving aggregation.

**Done when.** On the three-cluster harness, a distributed ActiveActive herd spreads by weight, and an ActivePassive herd runs `replicas` in the holder and `standbyReplicas` elsewhere, with `status.sites` live.

### C5. Lease follower

**Context.** Design Section 5.3; the failover service plan's F6 client library and Section 5 consumer contract. The trigger comes from the PlacementPolicy `failoverPolicy.trigger`.

**Files.** `pkg/controller/cattleset/lease_follower.go`; dependency on the client library module (orientation (4)).

**Implement.** One shared watch per distinct trigger (Global, or `primary-dc-<group>`), fanned out to every CattleSet on that trigger; `OnPrimaryDCChanged` re-queues the herd with the new holder as posture input; fail closed (library semantics: unreadable means not-primary, so a partitioned hub stops retagging and a partitioned site's herd is treated as standby by the spoke gate); per-DC health Leases feed the ActiveActive surge input; no second grace knob, the Lease's `leaseDurationSeconds` is the hysteresis.

**Tests.** Envtest with a scripted Lease: holder flip converges posture within one resync; flapping holder does not thrash (posture recomputes only on settled holder); losing Lease read access freezes posture and sets `LeaseAccessible: False`.

**Done when.** Killing the harness's active DC moves the Lease and the herd's posture flips without any spec or Git change (assert `metadata.generation` is unchanged).

### C6. Spoke gate agent (the write-path gate)

**Context.** Design Section 6: the rendered Service of a gated herd starts unadmitted (admission selector term selects nothing); the spoke agent patches the term out only while every gated data tier is the promoted, writable primary in this site, and restores it on demotion. Spoke-local, hub-independent.

**Files.** A small controller shipped from this repo as the cattleset spoke agent, or riding the existing spoke applier (orientation (5)): watches the local rendered Services (by CattleSet identity labels) and the local data tier objects via unstructured duck-typing of `status.disasterRecovery` (orientation (7)).

**Implement.** Admission = one atomic Service patch. A Service `selector` is an atomic map, so per-key ownership is impossible: after creation the agent owns the whole selector, relying on the C4 `ignoreFields` wiring so the work agent never reverts the flip. Revoke on any gated tier reporting demoted, unknown, or absent (fail closed); events `SiteAdmitted` / `SiteRevoked` with the tier states; `status.sites[].gated` fed back through the ManifestWork feedback (project the Service's admission state or mirror it in a reported condition).

**Tests.** Envtest: admit only when all tiers promoted; revoke on demotion; fail closed on missing tier; a work-agent resync of the ManifestWork does not revert an admitted Service (the ignoreFields wiring); hub unreachable does not block admission (agent has no hub client at all, prove by construction); the admitted flip does not restart pods (same pod template hash before and after).

**Done when.** On the harness, DR's Service has no endpoints until the fake Database flips to promoted in DR, then endpoints appear within the agent resync, with the hub network-partitioned during the flip.

### C7. Traffic rendering (ServiceExport, HTTPRoute, GSLB)

**Context.** Design Sections 4.1 (CattleTraffic), 5.2, 5.3 step 4: routes bind locally and never need repointing; GSLB primary tag follows the holder; steering is never fencing.

**Files.** `pkg/controller/cattleset/traffic.go`.

**Implement.** `traffic.export`: render a KubeSlice `ServiceExport` (slice from the PlacementPolicy) per Member site into the site's ManifestWork. `traffic.httpRoute`: render one HTTPRoute per site, `parentRefs` as given, hostnames defaulting to the GSLB host, backend the local Service. `traffic.gslb`: render the k8gb `Gslb` object (unstructured, no k8gb module dependency) per site, strategy `failover` with `primaryGeoTag` = current holder for ActivePassive, `roundRobin` for ActiveActive; retag on holder change is a hub-side render update, explicitly a client-experience action ordered after promotion, never a gate.

**Tests.** Golden rendered objects per mode; holder flip retags Gslb only (Deployment and Service unchanged); HTTPRoute is identical before and after a failover (prove the no-repoint property).

**Done when.** Harness DNS (k8gb in the two kind Members) answers the active site while healthy and the survivor after a DC kill, and the HTTPRoute objects never changed.

### C8. Cross-site rollout sequencing

**Context.** Design Section 7: StandbyFirst (ActivePassive default), OneSiteAtATime (ActiveActive default), Parallel; the admission gate does not touch pod readiness, so standby rollouts complete normally.

**Files.** `pkg/controller/cattleset/rollout.go`.

**Implement.** Compute the pod-template revision hash; per `rolloutOrder`, hold back sites' template updates until the previous site's rendered Deployment reports the new revision available (feedback `updatedReplicas` == desired and `availableReplicas` >= desired minus the strategy's `maxUnavailable`); `paused` blocks template propagation but never posture; `progressDeadlineSeconds` per site surfaces a `Progressing: False` site and pauses the sequence, not the posture.

**Tests.** Envtest with scripted feedback: StandbyFirst never touches the active site before the standby converges; a wedged standby stops the sequence but a concurrent Lease flip still executes the posture change on the old template (failover beats rollout); revision skew visible in `status.sites[].revision`.

**Done when.** The harness rolls a bad image with StandbyFirst and the active site never runs it; a failover mid-rollout still completes.

### C9. The distribution flip (false to true)

**Context.** Design Sections 5.1 and 8: enabling DR is a spec flip with adoption, never a pod restart; true to false is rejected.

**Files.** `pkg/controller/cattleset/adopt.go`; webhook already rejects the reverse (C2).

**Implement.** Per orientation (6)'s answer: on flip, the local cluster's existing Deployment (same name, namespace, identity labels) becomes the local Member site's rendered output, adopted by rendering the identical spec core so the applier's server-side apply produces no pod-template diff (assert equal template hash); other Members are seeded normally; the herd enters the declared posture only after all sites report ready, until then the local site stays active regardless of mode (no traffic gap during enablement).

**Tests.** Envtest and harness: flip with live traffic, zero pod restarts on the original site (pod UIDs unchanged), seeded standby comes up gated where `gates` is set.

**Done when.** The harness runs the flip under load with no dropped endpoints on the original site.

### C10. Observability, packaging, docs

**Files.** Metrics and events in `pkg/controller/cattleset/`; the installer chart at `kubeops.dev/installer` `charts/cattleset` plus `apis/installer/v1alpha1/cattleset_types.go`, modeled on `charts/petset`; platform-chart integration; docs.

**Implement.** Metrics: posture flips total, observe-to-converge seconds (Lease change to PostureConverged), gate admit/revoke latency, per-site ready gauges. Events on every posture, admission, surge, and rollout-hold transition. RBAC additions (ManifestWork, ServiceExport, HTTPRoute, Gslb, Lease read). The installer chart: `charts/cattleset` in `kubeops.dev/installer`, mirroring `charts/petset` file for file: `Chart.yaml`, `OWNERS`, `README.md`, `doc.yaml`, `crds/apps.k8s.appscode.com_cattlesets.yaml`, `templates/` (deployment, RBAC, service, secret, cert-manager wiring, mutating and validating webhooks, PDB, network policy, servicemonitor, and the `ocm/addon` and `ocm/work-agent` templates carrying the spoke gate agent), `values.yaml` with a gate agent enable flag, and chart-values Go types in `apis/installer/v1alpha1/cattleset_types.go` generating `values.schema.json` and `values.openapiv3_schema.yaml` via the installer repo's `make gen` (schemas are generated, never hand-written). The chart does not ship the `PlacementPolicy` CRD: petset's chart owns it (orientation (2)). Finalize the documentation set: `README.md` becomes the full user guide (Deployment to CattleSet migration via `FromDeployment`, the two modes, the gate, a failover walkthrough), `TEST.md` covers the complete Section 3 matrix, `DESIGN.md` is reconciled against the implemented behavior, `DEV.md` is checked against what a new contributor actually needed; plus a row for CattleSet in `kubedb-branching-and-dr-alignment.md` and an X-track note in `kubedb-dc-dr-implementation-plan.md`.

**Done when.** A drill on the harness produces a readable event trail and the metrics needed to compute the realized workload RTO.

---

## 3. e2e matrix (three-cluster harness plus the failover service)

Reuse the DC-DR plan Section 4 harness (two Member kind clusters plus one Arbiter, OCM hub, KubeSlice or plain connectivity, the failover service deployed per its own plan). Rows, each per relevant mode:

1. Non-distributed parity: the C3 corpus behaves identically to Deployment; HPA binds; scale works.
2. ActiveActive: weighted spread, Member loss surge within the health-Lease staleness window, share restored on return.
3. ActivePassive: Lease flip converges posture, generation unchanged, realized RTO recorded.
4. Write-path gate: DR Service endpointless until the duck-typed Database reports promoted; admission with the hub partitioned; revoke on failback; a ManifestWork resync leaves an admitted Service admitted.
5. Returning site: comes back standby and unadmitted, no write path even with stale DNS pointing at it.
6. Rollouts: StandbyFirst bad-image containment; OneSiteAtATime blast radius; failover mid-rollout.
7. Distribution flip: adoption without pod restarts under load.
8. Traffic: k8gb failover answers; HTTPRoute never repointed; ServiceExport reachable across the slice.
9. Hub loss: posture frozen, gate still admits locally, ActiveActive traffic still fails over at the DNS layer.

---

## 4. Sequencing and PR breakdown

One task equals one PR, in order: PR0 C0 (repo bootstrap), PR1 C1 (types, pin, `FromDeployment`), PR2 C2 (webhook), PR3 C3 (non-distributed parity, shippable alone as a Deployment replacement), PR4 C4 (posture and rendering, harness bring-up), PR5 C5 (Lease follower; needs the failover service deployed), PR6 C6 (gate agent), PR7 C7 (traffic), PR8 C8 (rollout sequencing), PR9 C9 (distribution flip), PR10 C10 (observability, packaging, docs). C0 then C1 gate all; C3 and C4 are independent after C2; C5 to C9 depend on C4.

---

## 5. Global definition of done

- The hard rules of Section 0 are each enforced by at least one test: the DeploymentSpec pin, the posture-invariant failover (generation unchanged), no Deployment-controller fork (rendered objects only), the Service-selector gate with no pod readinessGate anywhere in the tree, spoke-local admission under hub partition, zero pods in Arbiter and Witness sites, the gates-mode and same-trigger webhook rejections, and fail-closed Lease behavior.
- The Section 3 matrix is green on the three-cluster harness for both modes and both postures.
- In `kubeops.dev/cattleset`: `make gen`, `make manifests`, and `make verify` are idempotent and clean; vendor is tidy (including the petset API dependency); files carry license headers; commits are signed off; the structure-parity check against petset (workflows, Makefile targets, hack scripts) passes; `kubeops.dev/petset` itself is unmodified by this work.
- Docs shipped: `README.md` (user guide and migration guide), `DESIGN.md` in sync with the implemented behavior, `TEST.md` covering the full matrix, `DEV.md` current, `AGENTS.md` / `CLAUDE.md` in place, and the alignment-doc and DC-DR-plan cross-references updated.
- The `charts/cattleset` chart in `kubeops.dev/installer` installs cleanly with `charts/petset` present (the `PlacementPolicy` CRD dependency), its values schemas are generated by the installer repo's `make gen`, and that repo's own lint and checks pass.
