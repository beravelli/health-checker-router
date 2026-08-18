# Adding a service to the health checker

A "service" here is a probe target: something the checker polls in one or more
regions to decide whether that region is healthy. This page covers the whole
lifecycle — registering one, choosing a probe type, wiring credentials, and
promoting it to failover.

There are two ways to add a service: the Admin tab, and the API. They do the
same thing; the UI is a form over the API.

---

## The one decision that matters: role

Every service has a **role**, and it is the difference between a service that
observes and a service that can move production traffic.

| Role | Effect |
| --- | --- |
| `healthcheck_only` | Probed and displayed. Excluded from the region's composite health score. Cannot influence routing. |
| `healthcheck_failover` | Probed, displayed, **and** folded into the region score that drives LB weights. |

New services default to `healthcheck_only`. This is deliberate. A probe with a
typo in its path returns 404 forever; if that probe had failover rights from
the moment it was created, registering it would quietly drain a healthy region.

**Register as monitor-only, watch it for a few probe cycles, then promote.**

---

## Adding one through the UI

Admin → *Add a service*. You need:

- **Name** and **runtime** — free text, shown in the service matrix.
- **Role** — leave on *Health check only* for now.
- **Regions** — tick each region and give the base endpoint the probe targets
  (`https://checkout.us-east-1.internal.example.com`). Leave a region unticked
  if the service isn't deployed there; the matrix shows "not deployed" rather
  than a false failure.
- **Check type** and its settings (below).
- **Timing** — interval, timeout, and the hysteresis thresholds.
- **CA bundle env var** — optional, for probes against an internal PKI.

Then Admin → *Configured services* → click the role tag to promote it once
you're satisfied.

---

## Probe types

### HTTP(S)

The default. Issues `GET {endpoint}{path}` and compares the status code.

| Field | Meaning |
| --- | --- |
| `path` | Defaults to `/healthz` |
| `expectedStatus` | Defaults to `200` |
| `degradedAboveMs` | Optional, and **calibrate it against where the probe runs** — see below. A response slower than this is *degraded*, not healthy |

A transport error (connection refused, TLS failure, timeout) is recorded as
**unknown**, not critical. See [Unknown vs critical](#unknown-is-not-critical).

> **`degradedAboveMs` and probe placement.** This threshold turns latency into a
> `degraded` status, which scores 0.5 instead of 1.0 and sheds traffic. It
> therefore measures the *path* as much as the service. A value tuned while
> probing from inside the region will fire permanently once the checker runs
> from elsewhere, because every measurement then carries inter-region RTT —
> a standing false degradation that quietly moves traffic for no reason.
>
> Leave it unset unless you have deliberately recalibrated for the probe's
> actual vantage point. Latency as a *performance* signal is better served by
> dedicated per-region agents that have nothing to do with failover; this field
> exists to catch a service that is pathologically slow, not to measure
> milliseconds.

### TCP

Dials `host:port`. `probeConfig.port` is required; the host comes from the
region endpoint.

A refused connection is *critical* — something actively said no. A dial
*timeout* is *unknown*, because a black hole tells you nothing.

### gRPC

Calls `grpc.health.v1.Health/Check` over the endpoint's HTTP/2 transport.
Set `grpcService` if your server registers a named service.

### AWS ALB / NLB

Queries `DescribeTargetHealth` on a target group rather than probing the
service directly. Useful when the load balancer already knows more than an
external probe would.

Requires `probeConfig.targetGroupArn` and an **AWS auth method** (below).

The mapping is graded, not binary:

| Target state | Result |
| --- | --- |
| All targets healthy | healthy |
| Some healthy, ≥50% | degraded |
| Some healthy, <50% | critical |
| No healthy targets | critical |
| Everything draining or initial | unknown |
| API call failed | unknown |

That last row matters: losing AWS API access says nothing about your targets.

### Custom

Shells out to a command. Exit `0` = healthy, `1` = degraded, anything else =
critical. A command that can't start at all is unknown.

The binary **must** be listed in `SSO_BINARY_ALLOWLIST`. Anything else is
refused — otherwise the Admin form would be a remote code execution primitive
for anyone in the admin group.

---

## AWS authentication

ALB/NLB probes need AWS credentials. Four methods, and in every case what gets
stored is a **reference**, never a credential:

| Method | `awsAuthRef` holds | Notes |
| --- | --- | --- |
| `assume_role` | Role ARN | Uses the pod's own identity (IRSA) to assume it. Add `probeConfig.awsExternalId` if the trust policy requires one. Sessions are 15 minutes, refreshed by the SDK. |
| `service_account` | **Name of an env var** | The variable holds either `AKID:SECRET[:TOKEN]` or credential_process JSON. Populate it from Vault/ESO. |
| `aws_sso` | SSO profile name | Reads the cached SSO token. Expiry surfaces as an auth error; re-run `aws sso login`. |
| `custom_sso_binary` | Path to a binary | For orgs with an internal SSO wrapper. Same contract as the AWS CLI's `credential_process`: print JSON credentials to stdout. Must be on `SSO_BINARY_ALLOWLIST`. |

`assume_role` with IRSA is the right default on EKS.

---

## Timing and hysteresis

| Field | Default | What it does |
| --- | --- | --- |
| `probeIntervalS` | 5 | Seconds between probes |
| `probeTimeoutS` | 3 | Per-probe timeout; must not exceed the interval |
| `consecutiveFailuresToTrip` | 3 | Failures in a row before the target is marked critical |
| `consecutiveSuccessToRecover` | 2 | Successes in a row before it recovers |

The two thresholds are the flapping defence. With 3-to-trip, a probe
alternating pass/fail never trips at all, because each pass resets the counter.
Setting `consecutiveFailuresToTrip: 1` disables that protection — don't, unless
the target genuinely has no transient failure mode.

Between the first failure and the trip, the target shows as **degraded**. That
gives you a visible early warning without a routing change.

---

## Unknown is not critical

The checker distinguishes three negative outcomes, and the distinction drives
different behaviour:

- **degraded** — working, but impaired. Reduces the region's score, so its LB
  weight drops proportionally. Never evicts a region.
- **critical** — confirmed broken. Can drive the region's weight to zero.
- **unknown** — the probe couldn't reach a verdict. Does *not* advance the trip
  counter and does *not* reset it. If enough targets in a region go unknown at
  once, quorum fails and the system enters fail-open: last-known-good weights
  are held for up to 10 minutes and on-call is paged.

Fail-open exists because pulling a region on an unverified signal is worse than
waiting. See `docs/admin.md` for tuning the quorum policy.

---

## Adding one through the API

```bash
curl -X POST https://health-checker.example.com/api/admin/services \
  -H 'Content-Type: application/json' \
  --cookie "$SESSION" \
  -d '{
    "name": "checkout-service",
    "runtime": "EC2 ASG",
    "role": "healthcheck_only",
    "probeType": "http",
    "probeConfig": { "path": "/healthz", "expectedStatus": 200 },
    "probeIntervalS": 5,
    "probeTimeoutS": 3,
    "consecutiveFailuresToTrip": 3,
    "consecutiveSuccessToRecover": 2,
    "regions": {
      "us-east-1": "https://checkout.us-east-1.internal.example.com",
      "us-west-2": "https://checkout.us-west-2.internal.example.com"
    }
  }'
```

Promote it once you've watched it:

```bash
curl -X PATCH https://health-checker.example.com/api/admin/services/$ID \
  -H 'Content-Type: application/json' --cookie "$SESSION" \
  -d '{"role": "healthcheck_failover"}'
```

Remove it:

```bash
curl -X DELETE https://health-checker.example.com/api/admin/services/$ID \
  --cookie "$SESSION"
```

All three require membership in the admin LDAP group and are recorded in the
audit log.

**Probing starts immediately.** Creating or deleting a service reconciles the
probe fleet in place; no restart, no waiting for the next tick.

---

## An ALB example, end to end

```json
{
  "name": "checkout-service",
  "runtime": "EC2 ASG behind ALB",
  "role": "healthcheck_only",
  "probeType": "aws_alb",
  "probeConfig": {
    "targetGroupArn": "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/checkout/abc123",
    "awsExternalId": "hc-external-id"
  },
  "awsAuthMethod": "assume_role",
  "awsAuthRef": "arn:aws:iam::123456789012:role/health-checker-probe",
  "probeIntervalS": 10,
  "probeTimeoutS": 5,
  "regions": { "us-east-1": "", "us-west-2": "" }
}
```

The endpoint is empty because an ALB probe talks to the AWS API, not to the
service. The IAM role needs `elasticloadbalancing:DescribeTargetHealth`.

---

## Troubleshooting

**Service shows "unknown" forever.** The probe can't run. Check the API logs
for `probe could not run` or `skipping target with invalid configuration` — a
missing `targetGroupArn`, an unresolvable CA env var, or a custom binary not on
the allow-list all land here.

**Service is healthy but the region isn't.** Only `healthcheck_failover`
services count toward the region score, and dependencies contribute 40% of it.
Check the dependency tags on the region card.

**Probe passes manually but fails here.** If `caCertSecretEnv` is set, the probe
trusts *only* that bundle — the system trust store is not consulted. Either add
the issuing CA to the bundle or clear the field.

**Promotion to failover didn't change anything.** Weights only move while
automatic control is on. If someone set weights manually or triggered a
failover, the split stays put until *Reset to automatic*.
