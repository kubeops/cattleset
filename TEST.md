# TEST.md

How to test `kubeops.dev/cattleset`.

## Unit tests

```
go test ./...
```

Or via the Docker harness: `make test` (alias `make unit-tests`). Key suites:

- `apis/apps/v1/deployment_pin_test.go` the DeploymentSpec mirror pin (reflection over the vendored `apps/v1.DeploymentSpec` and `DeploymentStatus`).
- `pkg/controller/gate`: `admit_test.go` is the admission function, one case per check of the design (marker active here and fresh, not quiescing, a writable primary here, break-glass), the camps, the 15 s primary-loss hysteresis, and the closed-by-default parsing of the gate annotation. `agent_test.go` runs the agent against a fake spoke: admit, marker going stale mid-run, quiesce, an intra-DC primary change inside the grace, an agent restart during a loss, break-glass, closed without a usable gate, a leaderless tier, the heartbeat write rate, and the event mapping.
- `pkg/controller/cattleset`:
  - `ordering_test.go`: the camp table, the derived mode and gate (every path that must render **Closed**, and the tighten-only rule), ordering resolution, and the capacity rules (an unreadable or expired Lease holds capacity).
  - `switchover_test.go`: the planned-switchover plan, every exit clearing its annotation, and the `ready` definition with the full planned-switchover trace.
  - `reconcile_test.go`: the real `Reconcile` against a fake hub (`harness_test.go`): closed gates, held capacity, scale up early and down late, finalizer and deletion, pruning, pod-dependency delivery and the missing-source case, a dead DC never ready, the old holder staying ready after the database moves, the handoff request, abort and timeout, retiring the hub-local workload, and the Lease event filter and mapping.
  - `posture_test.go`, `rollout_test.go`, `render_test.go`, `traffic_test.go`, `manifest_work_control_test.go`, `adopt_test.go`: the pure engines, including the serving site always rolling last and a sitePatch-only change being sequenced.
- `pkg/webhooks/apps/v1` table-driven accept and reject per rule (removed fields, ports, marker age, `matchLabels`, `syncResources`, ActiveActive over a single writer), defaulting, update rules, and the warnings.

The fake client cannot do server-side apply, so the reconcile tests use a small shim that applies as create-or-replace and merges the herd's FailoverGroup member entry. It is a stand-in for the part of server-side apply the tests rely on, not a model of field ownership.

## What the unit tests do not cover

These need a real cluster, and are listed in `DESIGN.md` section 12: the database's Service being empty on the standby spoke, a non-slice pod reaching the database Service, the OCM work agent resolving the annotation JSONPath feedback, the joined planned-move path with dr-controlplane and the agents, and every timing figure.

## Running the gate agent and hub controller against a real API server

`hack/e2e-kind.sh` stands up one kind cluster as a hub and a spoke at once, with the OCM `ManifestWork` CRD and petset's CRDs installed but no OCM agents, and plays the parts nothing else supplies: it applies the ManifestWork's Service the way the work agent would, writes the marker ConfigMaps by hand, and fakes the database Service and its EndpointSlice. It exercises what fakes cannot: real server-side apply of the FailoverGroup member entry, the agent's label-scoped cache and RBAC, leader election, and real patch semantics. It is not the joined DC-DR path; that needs OCM, the dr-controlplane agents and a database.

## The three-cluster e2e matrix

Two Member clusters plus the Arbiter hub, OCM, dr-controlplane (hub controller and per-DC agents), and a DC-DR Postgres. To run once the harness is up:

1. Non-distributed parity: the corpus behaves identically to Deployment; HPA binds; scale works.
2. A herd over Postgres: the standby DC unadmitted, the active DC admitted only once the primary is `role=primary`.
3. Unplanned DC loss: the survivor scales up early, admits when the database is writable, the group reaches `Steady`; record the timings.
4. Planned database switchover and failback: the old holder stays ready, the FailoverGroup controller sets `switchover-to`, the herd sets `handoffTo`, the Lease moves.
5. Intra-DC primary failure: the gate holds through the grace.
6. A hub outage: admission keeps working, capacity and reporting freeze.
7. Break-glass on one DC: the application follows.
8. Aborted switchover and a stalled promotion.
9. Gate agent down: the last state persists and `GateAgentStale` is raised.
10. Rollouts: the serving site last; a bad image contained; a switchover holds propagation.
11. Distribution flip: adoption without pod restarts under load.
12. Pod dependencies: namespace and secrets reach a fresh spoke; a missing source keeps the copies.

Each task that adds a suite updates this file.
