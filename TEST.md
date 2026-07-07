# TEST.md

How to test `kubeops.dev/cattleset`.

## Unit tests

```
go test ./...
```

Or via the Docker harness: `make test` (alias `make unit-tests`). Key unit suites:

- `apis/apps/v1/deployment_pin_test.go` the DeploymentSpec mirror pin (reflection over the vendored `apps/v1.DeploymentSpec` and `DeploymentStatus`).
- `apis/apps/v1` `FromDeployment` round-trip and fuzzer round-trip for the new types.
- `pkg/controller/cattleset/posture_test.go` the posture engine table tests (weighted splits, largest-remainder rounding, surge, holder flips, zero for Arbiter and Witness). Pure functions, no cluster required.
- `pkg/webhooks/apps/v1` table-driven accept/reject per validation rule, defaulting golden objects, immutability on update.

## envtest

Controller and gate-agent tests run against a `sigs.k8s.io/controller-runtime` envtest control plane (`setup-envtest` provisions the binaries). They cover: non-distributed Deployment parity (rollout, rollback, pause, scale via the subresource, HPA binding through `status.selector`), faked ManifestWork status feedback driving aggregation, the Lease follower, and the write-path gate.

## The three-cluster e2e harness

Reuses the DC-DR plan Section 4 harness: two Member kind clusters plus one Arbiter, an OCM hub, KubeSlice or plain connectivity, and the Common Cross-DC Failover Service deployed per its own plan. Run with `make e2e-tests` once the harness is up.

The e2e matrix:

1. Non-distributed parity: the corpus behaves identically to Deployment; HPA binds; scale works.
2. ActiveActive: weighted spread, Member loss surge within the health-Lease staleness window, share restored on return.
3. ActivePassive: Lease flip converges posture, generation unchanged, realized RTO recorded.
4. Write-path gate: DR Service endpointless until the duck-typed Database reports promoted; admission with the hub partitioned; revoke on failback; a ManifestWork resync leaves an admitted Service admitted.
5. Returning site: comes back standby and unadmitted, no write path even with stale DNS.
6. Rollouts: StandbyFirst bad-image containment; OneSiteAtATime blast radius; failover mid-rollout.
7. Distribution flip: adoption without pod restarts under load.
8. Traffic: k8gb failover answers; HTTPRoute never repointed; ServiceExport reachable across the slice.
9. Hub loss: posture frozen, gate still admits locally, ActiveActive traffic still fails over at the DNS layer.

Each task that adds a suite updates this file.
