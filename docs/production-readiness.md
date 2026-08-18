# Production readiness

Honest status of this system against what it would take to let it move real
traffic. Kept in the repo rather than a ticket because the remaining gaps are
the kind that get forgotten once the code looks finished.

**Current status: not production ready.** The architectural blocker is fixed and
the system is now observable, but it has never run against a real database or a
real routing provider. See [Remaining](#remaining) for what stands between here
and a go-live.

---

## Done

### Single-writer election

Previously every replica ran the full pipeline. With `replicaCount: 2` and an
HPA to 6, that meant up to six processes independently probing every target and
six concurrent writers racing on the same weighted DNS records. Nothing in the
pipeline is idempotent enough for that to be safe.

`internal/leader` elects one replica through a Postgres advisory lock. The
leader runs the probe fleet, the evaluate loop, and the hourly maintenance;
standbys serve the console and the read API. Probe goroutines are parented to
the leadership term, so a replica that loses the lock stops probing.

Advisory locks rather than a Kubernetes lease because the database is already a
hard dependency and a single point of consistency, while the Kubernetes API is
neither — and a leader that cannot reach Postgres cannot do its job anyway.
`pg_advisory_lock` is session-scoped, so release on crash is the server's
problem: no lease to expire, no clock to trust.

Failover is bounded by `LEADER_POLL_INTERVAL` (default 5s).

### Observability, and the dead man's switch

`/metrics` exposes the pipeline: `hcr_region_score`, `hcr_region_weight`,
`hcr_region_unknown_ratio`, `hcr_region_trusted`, `hcr_fail_open_active`,
`hcr_probes_total`, `hcr_probe_duration_seconds`, `hcr_weight_changes_total`,
`hcr_leader`, `hcr_leader_terms_total`.

The one that matters most is `hcr_last_evaluation_timestamp_seconds`.

Every other signal here is emitted by a process that is working. A replica
isolated from the network cannot page, cannot write to Postgres, and cannot
serve `/readyz` — so from the outside, "nothing is wrong" and "the thing that
tells you something is wrong is dead" are indistinguishable. Only an alert on
*absence* separates them:

```yaml
- alert: HealthCheckerStalled
  expr: time() - max(hcr_last_evaluation_timestamp_seconds) > 180
  labels: { severity: critical }
```

`monitoring.prometheusRule.enabled=true` ships that rule plus no-leader,
leadership-flapping, sustained-fail-open, region-blind, region-critical, and
weight-push-failing alerts. `monitoring.serviceMonitor.enabled=true` sets up
scraping. Both need the Prometheus Operator CRDs, so both default off.

**Alertmanager must deliver these over a path that does not depend on this
cluster.** Otherwise the isolation that silenced the checker silences the alert
about it, and the dead man's switch is decorative.

### CI

`.github/workflows/ci.yml` runs on every push and PR, plus weekly for
advisories that appear without anyone pushing:

| Job | What it catches |
| --- | --- |
| `go` | tidy drift, gofmt, build, vet, `go test -race` |
| `integration` | store SQL and leader election against a real Postgres |
| `security` | `govulncheck` (reachability-based) and `npm audit` |
| `web` | typecheck and production bundle |
| `chart` | migration drift, lint, both value sets render, **and the render guards still reject invalid config** |
| `docker` | the multi-stage image still builds on the pinned toolchains |

### Test coverage

77 tests, up from 5. The additions target the paths where being wrong is
expensive:

- `internal/leader` — one leader across five candidates, failover on exit, lock
  release, term counting. Integration-tagged, because the mechanism *is*
  Postgres's advisory-lock semantics and a mock would only prove we call the
  mock.
- `internal/api` — the admin gate. Exact group matching against six near-misses,
  handler-did-not-run assertions, session resolution, forged cookies, CORS
  origin echo, `authDisabled` still honouring `DEV_GROUPS`.
- `internal/health` — the 60/40 split, monitor-only services being unable to
  move traffic, unknown-excluded-not-counted, classify boundaries, no dilution
  when only one side has observations.
- `internal/store` — integration-tagged: partition routing on `probe_results`,
  `EnsurePartitions` idempotency, weight round-trips, fail-open lifecycle,
  incident dedupe, new services defaulting to monitor-only.
- `internal/obs` — the heartbeat advances on both successful and erroring
  cycles, and leadership is read at scrape time.

### Earlier fixes

Three defects in the checker-side network failure path (quorum tolerance off by
one, undebounced unknowns, cold start seeding healthy) and three that made Helm
deployment impossible. See `git log`.

---

## Remaining

Ordered by what I would not go live without.

### 1. The routing adapters have never made a real API call

~1,100 lines across Route 53, CloudFront, Avi GSLB and Akamai, written against
remembered API shapes and never executed against a live endpoint. They are also
the only components that touch production traffic. Compiling against the AWS SDK
proves the signatures, nothing more.

This is now the largest single risk in the system. Needed:

- Unit tests against recorded fixtures for each adapter
- A dry-run mode that logs the intended change without applying it
- Verification against a non-production hosted zone or distribution

### 2. Nothing has run end to end

No probe has executed, no weight has been pushed, no LDAP bind has happened. The
local playbook ([docs/local-deploy.md](local-deploy.md)) is derived from
rendered manifests, not from a run.

### 3. Still-untested packages

`internal/auth` (369 lines — LDAP, group resolution, sessions),
`internal/awsauth` (382 lines — four credential paths), `internal/rollout`,
`internal/incident`, `internal/routing`. LDAP is the one to prioritise: the
admin gate is tested, but the group *resolution* feeding it is not.

### 4. Single vantage point

See [deployment-architecture.md](../design/deployment-architecture.md) for the
full failure matrix; scenarios 7 and 8 are the ones this gap makes harder to
interpret.

Quorum is across targets within a region, not across observers. One checker
loses its route to `us-west-2` and sees `us-west-2` go unknown; no tuning
distinguishes that from `us-west-2` actually being unreachable. Two additions
close it, in increasing order of cost:

- **A witness probe** — each cycle, probe something known-good and independent
  of the monitored services. All targets unknown *and* witness unknown means the
  checker is the problem, and the page should say so rather than making on-call
  infer it. This is a few hours of work and closes most of the gap.
- **A second checker instance elsewhere, cross-probing** — turns a single
  vantage point into real observer quorum, which is what "quorum" implies in the
  README today.

Note what this is *not*: it is not an argument for running probes inside the
monitored regions. See [ADR-001 decision 1](../design/ADR-001-multi-region-topology.md).
Probing from outside is fine, and arguably better, because it exercises the same
path customer traffic takes. What is missing is a *second* path, not a closer
one.

### 4a. Latency as a health signal is opt-in — configure it deliberately

`degradedAboveMs` turns a slow-but-successful response into `degraded`, which
scores 0.5 and sheds traffic. It measures the network path as much as the
service, so a threshold tuned while probing in-region fires permanently once the
probe runs from elsewhere.

`latencyBaselineMs` subtracts the known path RTT before the comparison, which
makes the option correct from any vantage point. Both default to zero, so
latency affects nothing until you switch it on. See
[adding-services.md](adding-services.md) for calibration.

### 5. Operational gaps

- **No runbook.** Alerts exist; documented responses do not.
- **No dashboard.** Metrics exist; a Grafana board does not.
- **No load testing.** Behaviour at hundreds of targets is unknown — probe
  goroutine count scales linearly with targets on a single pod.
- **`emptyDir` Postgres in the local path only.** Production needs managed
  Postgres or CloudNativePG.

### 6. Shadow run

The step that actually earns the label. Run against production targets in
monitor-only mode for several weeks with alerts live and weight pushes disabled.
Compare what it *would* have done against what actually happened. The design
supports this natively — every service starts monitor-only — and it is the only
way to find the bugs that reading and unit tests will not.

---

## Rough sequence

| Phase | Work | Rough effort |
| --- | --- | --- |
| 1 | Adapter fixtures + dry-run mode | ~1 week |
| 2 | Run the local playbook; fix what it surfaces | 1–2 days |
| 3 | LDAP and awsauth tests | ~3 days |
| 4 | Runbook + Grafana dashboard | ~3 days |
| 5 | Witness probe | ~2 days |
| 6 | Shadow run in monitor-only | 2–4 weeks, mostly waiting |

Phases 1–5 are a few weeks of work. Phase 6 cannot be compressed, and skipping
it means the first real test of this system is an outage.
