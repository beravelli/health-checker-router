# Active-Active Health Checker & Routing Console

A monitoring and control plane for an active-active microservices deployment
across `us-east-1` and `us-west-2`. It probes services and dependencies in both
regions, folds the results into a composite health score, drives load-balancer
weights from that score, and gives on-call SREs one place to see regional
health, control failover, track rollouts, and read the incident timeline.

Go backend, React frontend, Postgres, Helm chart.

---

## The idea in one paragraph

Most health checkers answer "is it up?" — a binary that produces false outages.
This one answers two separate questions: *how healthy is it* (a score, which
moves traffic proportionally) and *do we actually know* (a confidence check,
which decides whether to act at all). A region with a degraded cache loses some
traffic; it isn't evicted. A region whose probes are timing out isn't declared
dead — the system holds the last known good weights, pages on-call, and waits.
Guessing wrong about a healthy region is worse than waiting for a human.

---

## Design principles

**1. Weighted, not binary.** A region's score combines failover-eligible
service health (60%) with dependency health (40%). Degraded inputs reduce the
score; the LB weight follows. Only a genuinely critical score evicts a region.

**2. Hysteresis over single probes.** Three consecutive failures to trip, two
consecutive successes to recover. A flapping probe never moves traffic, because
each pass resets the counter.

**3. Unknown ≠ critical.** A probe timeout, a failed AWS API call, or a network
partition produces `unknown`, which does not advance the trip counter. When too
many targets in a region are unknown, quorum fails and the system fails open.

**4. Fail open, and say so.** Inconclusive signal means: hold last-known-good
weights up to 10 minutes, page on-call, and show the banner. Escalate to
critical if the hold expires. Never fail over on an unverified signal.

**5. Not DNS, and not the Route 53 control plane either.** DNS TTL caching means
a record change can take minutes to reach clients, so weight changes go through
the edge (Envoy / the routing provider's fastest mechanism) with DNS TTL pinned
to 30s as a backstop. Record *writes* have a second problem: they are
control-plane calls hosted in us-east-1, so evacuating us-east-1 can depend on
us-east-1. Evacuation therefore goes through ARC routing controls, whose data
plane spans five regions. See [ADR-001](design/ADR-001-multi-region-topology.md).

**6. References, not secrets.** Nothing in Postgres is a credential. Every
credential field stores the *name* of an environment variable or a secret path,
resolved at call time. Missing means fail closed, never fall back.

**7. Safe by default.** A newly registered service is monitor-only. Giving it
the power to move production traffic is an explicit second step.

---

## Layout

```
cmd/api/                  Entrypoint: probe fleet, evaluate loop, HTTP server
internal/
  prober/                 Probe implementations + hysteresis state machine
  health/                 Composite region and service scoring
  failopen/               Quorum evaluation and the fail-open window
  lb/                     Weight computation, the only writer of LB state
  routing/                Provider interface + registry
    awsnative/            Route 53 weighted records
    cloudfrontadapter/    CloudFront origin-group failover
    avigslb/              Avi GSLB pool member ratios
    akamai/               Akamai ALB Cloudlet policy versions
    arc/                  AWS Application Recovery Controller routing controls
  awsauth/                AWS credential resolution (4 methods) + ELBv2 health
  auth/                   LDAP directory, group resolution, sessions
  incident/               Incident recording and on-call paging
  rollout/                Rollout tracking with canary gating
  api/                    REST + SSE handlers, middleware, admin gate, static console
  leader/                 Single-writer election over a Postgres advisory lock
  obs/                    Prometheus metrics, including the heartbeat
  store/                  Postgres persistence
  events/                 In-process pub/sub behind SSE
  models/                 Domain types
  config/                 Environment configuration + secret resolution
db/migrations/            goose SQL migrations
deploy/helm/              Helm chart
web/                      React + TypeScript dashboard
design/                   Original design handoff and screenshots
docs/                     Guides (below)
```

---

## Documentation

| | |
| --- | --- |
| [docs/adding-services.md](docs/adding-services.md) | **How to add a service** — probe types, AWS auth, hysteresis tuning, troubleshooting |
| [docs/admin.md](docs/admin.md) | **Admin functionality** — the LDAP gate, regions, routing providers, fail-open policy, audit log, and how to add new admin capabilities |
| [docs/api.md](docs/api.md) | Full API reference |
| [docs/local-deploy.md](docs/local-deploy.md) | **Local deploy playbook** — build the image and run the whole stack on a local cluster via the Helm chart |
| [docs/production-readiness.md](docs/production-readiness.md) | **Production readiness** — what is done, what is missing, and what it would take to go live |
| [design/ADR-001](design/ADR-001-multi-region-topology.md) | **Multi-region topology** — where the checker itself should run, why the controller is deliberately not active-active, and why evacuation goes through ARC |

---

## Running locally

Prerequisites: Go 1.25+, Node 20+, Docker, and
[goose](https://github.com/pressly/goose) for migrations.

```bash
# 1. Resolve Go dependencies (writes go.sum)
make tidy

# 2. Start Postgres
make db-up

# 3. Apply migrations — creates the schema and seeds both regions
make migrate

# 4. Run the API with auth disabled (every request is a local admin)
make run

# 5. In another terminal, run the dashboard
make web-install
make web-dev
```

Open http://localhost:5173. The Vite dev server proxies `/api` to `:8080`, so
the session cookie works without CORS being involved.

That is the fast inner loop. To run the real container on a local Kubernetes
cluster through the Helm chart instead — which is what CI and production
deploy — follow [docs/local-deploy.md](docs/local-deploy.md), or:

```bash
kubectl create namespace health-checker
kubectl apply -f deploy/local/postgres.yaml
make local-up
kubectl -n health-checker port-forward svc/health-checker 8080:80
```

You'll land on an empty Overview — both regions exist but nothing is registered
yet. Go to **Admin → Add a service** and point a probe at something.

`make check` runs vet, the Go tests, and the frontend build.

---

## Configuration

Everything comes from the environment. See [`.env.example`](.env.example) for
the annotated list. The ones that matter:

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | — | Required |
| `PROBE_INTERVAL` | `5s` | Global default; per-service values override |
| `PROBE_TIMEOUT` | `3s` | Clamped to `PROBE_INTERVAL` |
| `EVALUATE_INTERVAL` | `15s` | How often health is recomputed and weights reconciled |
| `LEADER_POLL_INTERVAL` | `5s` | Advisory-lock retry interval; bounds leader failover time |
| `WEB_DIR` | `/app/web` | Console bundle directory; empty runs the API headless |
| `FAILURES_TO_TRIP` | `3` | Confirmed failures before a target trips |
| `SUCCESS_TO_RECOVER` | `2` | Good probes before a target is healthy — including its first verdict after startup |
| `UNKNOWNS_TO_DISTRUST` | `3` | Inconclusive probes before a target counts against quorum |
| `QUORUM_REQUIRED` / `QUORUM_TOTAL` | `2` / `3` | Fail-open quorum. Tolerated blindness is `(TOTAL-REQUIRED)/TOTAL`; `REQUIRED` must be `< TOTAL` |
| `FAIL_OPEN_HOLD` | `10m` | How long last-known-good weights are held |
| `LDAP_URL`, `LDAP_BASE_DN`, … | — | Required unless `AUTH_DISABLED=true` |
| `LDAP_ADMIN_GROUP` | `sre-admins` | Group gating every mutation |
| `AUTH_DISABLED` | `false` | Local dev only. Logs a warning on every boot |
| `SSO_BINARY_ALLOWLIST` | — | Binaries the custom-SSO and custom-probe paths may execute |

---

## Deploying

```bash
helm upgrade --install health-checker deploy/helm \
  --set image.tag=v0.1.0 \
  --set config.ldapURL=ldaps://ldap.example.com:636 \
  --set config.ldapBaseDN='dc=example,dc=com' \
  --set ingress.enabled=true \
  --set ingress.hosts[0].host=health-checker.example.com
```

The chart gives you a 2-replica Deployment with liveness/readiness probes, an
HPA, a PodDisruptionBudget, a NetworkPolicy, and a pre-install/pre-upgrade
migration hook so the schema is never behind the code.

Replicas are safe because the probe fleet and the evaluate loop are elected, not
replicated: one pod holds a Postgres advisory lock and drives the pipeline while
the others serve the console and the read API. Scaling out adds read capacity
and failover, never a second writer.

Turn on scraping and alerts with `monitoring.serviceMonitor.enabled=true` and
`monitoring.prometheusRule.enabled=true` (both need the Prometheus Operator
CRDs). The rule set includes a dead man's switch on
`hcr_last_evaluation_timestamp_seconds` — deliver it over a path independent of
this cluster, or the outage that silences the checker silences the alert too.

The chart carries no database subchart. Helm validates declared dependencies
before conditions are evaluated, so bundling one would make `helm install` fail
until `helm dependency update` had succeeded — including for the deployments
that bring their own database, which is all of them. Point
`secrets.databaseSecretName` at your own Postgres instead.

Migrations are packaged from `deploy/helm/migrations`, a synced copy of
`db/migrations`, because Helm's `.Files` cannot read outside the chart
directory. Run `make helm-sync-migrations` after adding a migration; `make
check` fails if the two drift.

Two Secrets are **not** created by the chart, deliberately — populate them from
your secrets engine:

- `health-checker-db` with key `url`
- `health-checker-ldap` with key `bindPassword`

Provider credentials follow the same route via `secrets.extraEnvFrom`. See
[deploy/k8s/README.md](deploy/k8s/README.md).

On EKS, annotate the ServiceAccount with an IRSA role and use `assume_role` for
AWS probes — that avoids long-lived keys entirely.

---

## Edge cases handled

| Case | Behaviour |
| --- | --- |
| Single dependency degraded | Composite score drops; weight reduces proportionally, region not evicted |
| Flapping health checks | Hysteresis — 3 to trip, 2 to recover; a flapping probe never trips |
| Cross-region network partition | Health judged locally per region; regions never check each other, so no split-brain failover |
| DNS TTL propagation delay | Weight change applied at the edge immediately; DNS TTL pinned to 30s as a backstop only |
| Cascading retries | Degraded status is surfaced so the Envoy layer can disable retry-to-same-region |
| Inconclusive signal | Fail-open: last-known-good weights held up to 10m, on-call paged, banner shown |
| AWS API unreachable | Probe reports `unknown`, not `critical` — it says nothing about the targets |
| CloudFront can't do weighted splits | Adapter reports the 100/0 reality instead of pretending a ratio applied |
| Rollout resumed into an unhealthy canary | Blocked with 409 and the reason |

---

## Design source

`design/` holds the original handoff: the HTML prototype, the implementation
plan, and reference screenshots of all five tabs. The React build recreates
those screens against the real API — the design tokens live in
`web/src/theme.css` and nothing else references raw colour values, so swapping
in a real Organic token sheet is a one-file change.

The prototype's "viewing as SRE / SRE admin" nav toggle is intentionally absent
here. It existed to demo both states; in this build your role comes from the
authenticated LDAP session, and the server is the one enforcing it.
