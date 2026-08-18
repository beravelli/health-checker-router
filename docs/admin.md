# Admin functionality

Everything in the Admin tab, plus the access model behind it.

---

## Who can do what

Two levels, and only one of them is enforced by the server:

| | Read health data | Mutate |
| --- | --- | --- |
| Authenticated user | ✅ | ❌ |
| Member of the admin LDAP group | ✅ | ✅ |

The admin group is `LDAP_ADMIN_GROUP`, default `sre-admins`.

**Mutations** means more than configuration. Changing LB weights, triggering a
failover, and pausing a rollout are all admin-gated, because each of them can
take a region out of service just as effectively as deleting a probe.

### How the gate actually works

Every mutating route passes through `requireAdmin`, which checks LDAP group
membership resolved at login. The frontend hides admin controls when you lack
the group, but that is cosmetic — the server rejects the request either way,
returning:

```json
{ "error": "admin access requires LDAP group membership", "requiredGroup": "sre-admins" }
```

The `requiredGroup` field is why the access-restricted card names the correct
group without hardcoding it client-side.

Group memberships are cached for `LDAP_CACHE_TTL` (default 5 minutes) so a
lookup doesn't run on every request. Signing out invalidates the cache
immediately, so revoking someone's group and having them sign out takes effect
at once rather than after the TTL.

> The design prototype had a "viewing as SRE / SRE admin" toggle in the nav.
> That was a prototype affordance for demoing both states and is **not** in
> this build. Your role comes from the authenticated session.

### Local development

`AUTH_DISABLED=true` skips LDAP entirely and runs every request as `DEV_USER`
with `DEV_GROUPS`. It logs a warning on every startup. Never set it in an
environment that can reach production infrastructure.

---

## Managing regions

Admin → *Regions*.

| Field | Notes |
| --- | --- |
| Region id | Must match what your infrastructure calls it (`us-east-1`) — probe results, weights, and routing config all key off it |
| Display name | Shown in the UI |
| Provider | Free text (`AWS`, `GCP`, on-prem…) |
| Zone label | The kicker above the region name: "primary write region" |

Adding a region makes it available when registering services and gives it a row
in the weight table. It receives no traffic until a service is bound to it and
weights are recomputed.

**Removing a region cascades**: its probe history, service bindings, and weight
row all go. It is not reversible; the data is deleted, not archived.

---

## Managing services

Covered in depth in [adding-services.md](./adding-services.md). The short
version:

- *Add a service* registers a probe target. Defaults to monitor-only.
- The role tag in *Configured services* toggles between monitor-only and
  failover-capable. One click, immediate.
- *Remove* deletes the service and its probe history, and stops its probes.

Adding or removing reconciles the running probe fleet in place — no restart.

---

## Traffic routing provider

Admin → *Traffic routing provider*. This selects which external traffic manager
the checker pushes weight and failover changes to. **Exactly one is active**;
enabling one disables the rest.

### Validation on enable

Saving with *Save and enable* builds the adapter for real: it resolves the
credential reference and checks every required config field. A provider that
cannot authenticate is rejected at configuration time, with the error shown
inline.

This is intentional friction. The alternative is discovering that your Akamai
credentials expired at the moment you're trying to fail out of a burning
region.

Use *Save without enabling* to stage a config you're not ready to switch to.

### Provider config

Each provider takes a JSON config object.

**AWS Route 53 / ALB-NLB** (`aws_native`)

```json
{
  "hostedZoneId": "Z123456ABCDEF",
  "recordName": "app.example.com",
  "apiRegion": "us-east-1",
  "regionTargets": {
    "us-east-1": "east-alb.example.com",
    "us-west-2": "west-alb.example.com"
  }
}
```

Writes weighted record sets in a single change batch, so the split applies
atomically. TTL is pinned to 30s — DNS is the backstop here, not the primary
failover path (see the DNS TTL edge case).

**AWS CloudFront** (`cloudfront`)

```json
{
  "distributionId": "E1ABCDEF",
  "originGroupId": "app-origins",
  "regionOrigins": { "us-east-1": "east-origin", "us-west-2": "west-origin" }
}
```

⚠️ CloudFront origin groups are primary/secondary with no weighted split. Any
weight change collapses to "whichever region has the majority becomes primary",
i.e. an effective 100/0. The Failover tab reports it that way rather than
pretending a ratio was applied.

**AVI GSLB** (`avi_gslb`)

```json
{
  "controllerUrl": "https://avi.example.com",
  "gslbServiceName": "app-gslb",
  "username": "svc-health-checker",
  "tenant": "admin",
  "regionMembers": { "us-east-1": "10.0.1.10", "us-west-2": "10.1.1.10" }
}
```

Read-modify-writes the GSLB service object, updating member `ratio` values.
Members at ratio 0 are also disabled, since ratio alone leaves them eligible in
some Avi versions.

**Akamai Cloudlet** (`akamai_cloudlet`)

```json
{
  "policyId": "12345",
  "network": "production",
  "regionOrigins": { "us-east-1": "east_origin", "us-west-2": "west_origin" }
}
```

Cloudlet policy versions are immutable, so each change creates a new version and
activates it. Requests are EdgeGrid-signed.

### Auth references

The **auth reference** field never holds a credential. It holds a pointer:

| Provider | Auth method | Reference is |
| --- | --- | --- |
| `aws_native`, `cloudfront` | `assume_role` | Role ARN |
| | `service_account` | Name of an env var holding the credentials |
| | `aws_sso` | SSO profile name |
| `avi_gslb` | — | Name of an env var holding the API token or password |
| `akamai_cloudlet` | — | Name of an env var holding the EdgeGrid bundle as JSON (`client_token`, `client_secret`, `access_token`, `host`) |

The application resolves these with `os.Getenv` at call time. Populate the
variables from your secrets engine — Vault Agent, External Secrets Operator, the
Secrets Manager CSI driver — via `secrets.extraEnvFrom` in the Helm values.

If a referenced variable is unset or empty, the affected operation **fails
closed** with a clear error. It never falls back to ambient credentials, and the
resolved value is never logged.

---

## Fail-open policy

Not editable in the UI — it is deployment configuration, because changing it
changes how the system behaves during an incident and that should go through
your normal change process.

| Env var | Default | Meaning |
| --- | --- | --- |
| `QUORUM_REQUIRED` | 2 | Conclusive probes needed |
| `QUORUM_TOTAL` | 3 | Probes expected |
| `FAIL_OPEN_HOLD` | 10m | How long last-known-good weights are held |
| `FAILURES_TO_TRIP` | 3 | Confirmed failures before a target trips |
| `SUCCESS_TO_RECOVER` | 2 | Good probes before a target is healthy |
| `UNKNOWNS_TO_DISTRUST` | 3 | Inconclusive probes before a target counts against quorum |
| `EVALUATE_INTERVAL` | 15s | How often the decision loop runs |

`QUORUM_REQUIRED` must be strictly less than `QUORUM_TOTAL`. Equal values
tolerate zero inconclusive probes, so the first blip would distrust the region
and hold fail-open indefinitely; the server refuses to start on that config.

### When a region stops being trusted

The tolerated share of inconclusive targets is `(total - required) / total` —
one third at the 2-of-3 default. Above that, quorum is not met, the region's
signal is untrusted, and the system:

1. Suppresses automatic reweighting entirely — the last good split stands
2. Opens a fail-open window and pages on-call
3. Shows the banner
4. Pages again at critical severity if the window outlives `FAIL_OPEN_HOLD`

Manual operator actions still work throughout. Fail-open constrains what the
*system* will do on its own, not what you can do.

A target counts as inconclusive once it has returned `UNKNOWNS_TO_DISTRUST`
consecutive unknown probes, or while it is still at its startup state having
never produced a verdict. Both paths matter when the fault is on the checker's
side rather than the target's: a network problem local to the checker turns
every probe inconclusive simultaneously, and a checker restarted mid-incident
must not report health it has not measured.

### Tuning for a noisy network

If probes are inconclusive often enough that fail-open opens spuriously, raise
`UNKNOWNS_TO_DISTRUST` before touching quorum. It buys tolerance for transient
blips without widening how blind the system is willing to be while still moving
traffic. Widening quorum tolerance — raising `QUORUM_TOTAL` relative to
`QUORUM_REQUIRED` — does the opposite, and should be a deliberate decision.

The current values are visible read-only in Failover & routing.

---

## Audit log

Every admin mutation is recorded: actor (LDAP username), action, target, and
timestamp.

```bash
curl https://health-checker.example.com/api/admin/audit --cookie "$SESSION"
```

Actions recorded: `service.create`, `service.update_role`, `service.delete`,
`region.upsert`, `region.delete`, `routing.upsert`, `lb.set_weights`,
`lb.reset_to_automatic`, `lb.failover`, `rollout.pause`, `rollout.resume`,
`rollout.rollback`.

The log is append-only from the application's side — there is no delete
endpoint. Retention is up to you.

---

## Adding a new admin capability

If you're extending this, the pattern is four steps:

1. **Store method** in `internal/store/store.go`. Keep SQL out of the domain
   packages.
2. **Handler** in `internal/api/handlers.go`. Validate input into a domain type
   before touching the store; return 400 with a specific message on bad input.
3. **Route** in `internal/api/server.go`, wrapped in `s.requireAdmin`. If it can
   affect traffic, it is admin-gated — no exceptions.
4. **Audit call** via `s.audit(r, "noun.verb", target, detail)`.

Then add the UI in `web/src/tabs/Admin.tsx` and the client method in
`web/src/lib/api.ts`.

Two conventions worth keeping:

- **Store references, not secrets.** If your capability needs a credential, add
  a field for the env var name and resolve it with `config.Resolve` at use time.
- **Default to the safe state.** Anything that could move traffic should be
  opt-in, the way service role defaults to monitor-only.
