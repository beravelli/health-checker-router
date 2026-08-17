# Handoff: Active-Active Health Checker & Routing Console

## Overview
A monitoring/control console for an active-active (us-east-1 + us-west-2) microservices system on Envoy + EC2. Gives on-call SRE a single view of per-region health, service-level status matrix, LB failover/routing controls with fail-open policy, deploy rollout tracking, and an incident timeline.

## About the Design Files
The bundled file is a **design reference built in HTML** — a working prototype of look, layout, and interaction, not production code to copy directly. Recreate this design in the target codebase's existing environment (React, Vue, etc.) using its established component library and patterns. If no frontend stack exists yet, React is a reasonable default given the component-driven structure below.

## Fidelity
**High-fidelity.** Colors, type, spacing, and component styling are final (from the bound "Organic" design system). Recreate pixel-close using the codebase's own styling approach, substituting equivalent design tokens if the codebase has its own theme.

## Screens / Views
One page, five tab-switched views (no routing — client-side tab state `activeTab`).

### 1. Overview
- **Purpose**: At-a-glance health of both regions.
- **Layout**: Dismissible fail-open banner (full width) → 2-column grid (`grid-template-columns: 1fr 1fr`, gap 20px) of region cards → below, a "7-day uptime trend" card with one bar-chart row per region.
- **Region card** (clickable → opens region detail dialog): kicker label (zone), region name (24px heading), status pill (top-right), 4-column metric grid (traffic share %, p99 latency ms, error rate %, replication lag ms — each a 26px heading number over an 11px uppercase label), divider, row of dependency status tags (Postgres, Redis cache, Kafka, Envoy edge), meta line (version, 30-day uptime %).
- **Trend chart**: per region, 7 vertical bars (Mon–Sun) with height ∝ uptime value, bar color by health (sage = healthy ≥99%, terracotta = warning 97–99%, red = critical <97%).

### 2. Services (matrix)
- **Purpose**: Per-service health across both regions with deploy version skew.
- **Layout**: Single card wrapping a `.table`: columns Service, Runtime, us-east-1, us-west-2, Version skew, Notes. Status shown as colored tag pills per region column; "skewed" outline tag when versions diverge.

### 3. Failover & routing
- **Purpose**: Manual LB control + policy transparency + edge-case documentation.
- **Layout**: 2-column grid (1.2fr / 1fr). Left card: two linked range sliders (us-east-1 / us-west-2 weight, sum to 100, dragging one adjusts the other), "Reset to automatic" (secondary button) and "Trigger manual failover to west" (primary button, opens confirm dialog). Right card: fail-open policy explainer text + a definition list (probe interval 5s, consecutive failures to trip 3, quorum 2 of 3 AZ probes, fail-open hold last-known-good max 10m).
- Below: "Edge cases this system covers" — 6 accordion rows (click header to expand), each with a description paragraph and a "Mitigation" tag + text. Covers: single-dependency degradation, flapping health checks, cross-region network partition, DNS TTL propagation delay, cascading retries, inconclusive/fail-open signal.

### 4. Rollouts
- **Purpose**: Track per-service deploy progress across both regions.
- **Layout**: Stacked cards, one per rollout: header row (service name, version range, status pill), 2-column progress bars (east %, west %, pill-shaped, sage fill), footer buttons Pause/Resume and Roll back (roll back zeros west progress and pauses).

### 5. Incidents
- **Purpose**: Cross-region incident/notification timeline.
- **Layout**: Header row with description + a severity segmented control (All/Critical/Warning/Resolved) filtering the list below. Each row: time column (78px), colored vertical accent bar (by severity), title + severity tag + region tag, description text.

### Admin (LDAP-gated)
- **Purpose**: Register services for probing and set each one's role.
- **Layout**: A demo-only "viewing as" segmented control in the nav (SRE / SRE admin) simulates which LDAP group the signed-in user resolves to — in a real build this comes from the authenticated session, not a UI toggle. When not in `sre-admins`, the tab shows an access-restricted card naming the required group instead of the form. When allowed: an "Add a service" card (name, runtime, region checkboxes, role segmented control: Health check only vs. Health check + failover) and a configured-services table (role tag click toggles it, Remove button).

### Dialogs
- **Region detail** (opens from region card click): AZs healthy, nodes in service, envoy proxies, last probe time, then a dependency breakdown list with status tags.
- **Failover confirm**: warns that confirming routes 100% traffic to us-west-2, pulls east out of rotation, and pages on-call both regions. Cancel / "Failover to west" actions.

## Interactions & Behavior
- Tab switching: `activeTab` state, no page reload.
- Fail-open banner: dismissible, `bannerVisible` boolean.
- Region card click → dialog with `selectedRegionId`; click outside (backdrop) or Close button dismisses.
- LB sliders: native range inputs, linked (east + west = 100), "Reset to automatic" restores 55/45 default.
- "Trigger manual failover" → confirmation dialog → on confirm, sets weights to 0/100 and re-shows the fail-open banner.
- Edge-case accordion: single-expand (`expandedEdgeCase` id), click header to toggle.
- Severity filter: single-select segmented control over incident list.
- Rollout Pause/Resume: toggles `paused` + status label. Roll back: sets west progress to 0, marks paused.
- No animations beyond default browser transitions; all state changes are instant.

## State Management
- `activeTab`: 'overview' | 'services' | 'failover' | 'rollouts' | 'incidents'
- `bannerVisible`: boolean
- `lbEast` / `lbWest`: numbers summing to 100
- `showRegionDialog`, `selectedRegionId`
- `showFailoverConfirm`: boolean
- `expandedEdgeCase`: id or null
- `severityFilter`: 'all' | 'critical' | 'warning' | 'resolved'
- `rollouts`: array of `{ id, service, fromVersion, toVersion, status, eastPct, westPct, paused }`
- Static reference data (would come from a real health-check API/backend): `regions`, `services`, `edgeCases`, `incidents` — see the DC's logic class for full shape and current mock content.

## Design Tokens
From the Organic design system (`_ds/organic-.../styles.css`):
- Background `#f5ead8`, text `#201e1d`, surface `#ebddc5`
- Accent (terracotta) `#c67139` — ramp 100 `#fff2eb` → 900 `#402310`
- Accent-2 (sage) `#7a8a5e` — ramp 100 `#f0fae1` → 900 `#272e1b`
- Neutral ramp 100 `#f9f4ed` → 900 `#2e2b25`
- Custom "critical" status color (not in base system, generated in OKLCH to match the ramp style): background `oklch(93% 0.045 25)`, foreground `oklch(38% 0.15 25)`, base dot `oklch(58% 0.19 25)`
- Fonts: headings `Caprasimo` (400 weight only), body `Figtree` (400/600/700)
- Radii: sm 8px, md 16px, lg 28px (cards/dialogs round further to `28px * 1.15`); buttons/tags/inputs are pill (`999px`)
- Shadows: sm/md/lg per token sheet, tuned to the cream ground
- Status semantics: healthy = sage tint, degraded/warning = terracotta tint, critical = custom red tint

## Suggested Go backend structure
The UI implies a backend service, not just a static frontend. A reasonable Go layout:

- **Prober package** (`internal/prober`): one goroutine per (region, service, dependency) probe target, ticking every 5s (configurable). Each result feeds a small state machine per target implementing the hysteresis rule: 3 consecutive failures → degraded/critical, 2 consecutive successes → recover. Persist rolling probe history (e.g. ring buffer) for the uptime trend.
- **Health aggregator** (`internal/health`): combines dependency + service probes into the composite region/service score shown in the UI (`healthy` / `degraded` / `critical`), per the "single dependency degraded" edge case — weighted, not binary.
- **Quorum/fail-open logic** (`internal/failopen`): when probe quorum (2 of 3 AZ probes) can't be reached, hold last-known-good weights for up to the configured window (10m) instead of flipping to down; emit a `FailOpenActive` event for the banner + paging.
- **LB weight controller** (`internal/lb`): owns current Envoy weight state per region; exposes `SetWeight`, `ResetToAutomatic`, `TriggerFailover(region)`. Should push weight changes to Envoy via xDS (or your existing control plane) rather than DNS, per the DNS-TTL edge case.
- **Routing provider adapters** (`internal/routing/{awsnative,cloudfront,avigslb,akamai}`): the Admin page's "Traffic routing provider" setting selects which external traffic manager actually fronts the app, and `TriggerFailover`/weight changes should dispatch through the matching adapter behind a common interface (`SetWeight`, `Failover`, `Status`) rather than being hardcoded to one provider: **AWS native** → Route 53 weighted/failover record sets (`route53.ChangeResourceRecordSets`) plus ALB/NLB target-group health; **AWS CloudFront** → origin-group failover via `cloudfront.UpdateDistribution` (primary/secondary origin, failover status codes); **AVI GSLB** → Avi Controller REST API to update GSLB pool member ratio/priority for the configured `gslbServiceName`; **Akamai Cloudlet** → Akamai's Application Load Balancer Cloudlet API (`cloudlets` v3) to activate a new policy version that routes away from the unhealthy origin, keyed by the configured policy ID and property hostname.
  - Each provider has its own auth config in Admin, following the same "store a reference, not the secret" pattern used elsewhere: AWS native and CloudFront each pick Assume-role / Service-account / AWS-SSO, identical to the per-service AWS auth above (same `internal/awsauth` credential providers, just a distinct role/secret/SSO profile scoped to that provider's API surface — e.g. `route53:ChangeResourceRecordSets` vs `cloudfront:UpdateDistribution` permissions). AVI GSLB stores a controller username plus a secret-manager path to the API token/password, fetched at call time. Akamai stores a single secret path to the EdgeGrid `.edgerc`-equivalent bundle (client token, client secret, access token, host) — use `akamai/AkamaiOPEN-edgegrid-golang` for the signed-request auth once credentials are resolved.
- **Rollout tracker** (`internal/rollout`): tracks per-service/per-region deploy progress and canary state; `Pause`, `Resume`, `Rollback` actions gate promotion between regions.
- **Incident/notification service** (`internal/incident`): records incidents (region, severity, description), fires on-call pages when fail-open or a confirmed failure persists past threshold; exposes the timeline feed.
- **API layer** (`cmd/api` + `internal/api`): REST or gRPC endpoints mirroring the UI's data needs — `GET /regions`, `GET /services`, `GET /incidents?severity=`, `POST /lb/weights`, `POST /lb/failover`, `POST /rollouts/{id}/pause|resume|rollback`. Use SSE/WebSocket for live health updates instead of polling, since this is a live dashboard.
- **State model**: mirror the "State Management" section above as Go structs (`Region`, `ServiceStatus`, `Incident`, `Rollout`) with `Status` as a typed enum (`Healthy`/`Degraded`/`Critical`), not a raw string, matching the UI's tri-state semantics.
- **Auth/authz** (`internal/auth`): bind to your LDAP directory (`go-ldap/ldap/v3` is the common choice) to authenticate the signed-in user and resolve their group memberships on login/token-refresh, not per-request. Gate all admin mutation endpoints (`POST/PUT/DELETE` under `/admin/services`, plus the LB/failover/rollout mutation endpoints if you want the same gate there) behind membership in a configured admin group (e.g. `sre-admins`), enforced server-side as the source of truth — the UI's role switcher is a prototype stand-in only and must not be trusted for real access control. Return 403 with the required group name so the frontend can render the same "you need group X" messaging shown in the Admin tab's access-restricted state. Cache group membership briefly (e.g. 5 min TTL) to avoid hammering LDAP on every request; invalidate on logout.
- **Secrets**: every credential field in Admin (service-account creds, CA bundle, AVI API token, Akamai EdgeGrid bundle) stores an **environment variable name**, not a value — the app resolves it with `os.Getenv(name)` at call time. Populate those env vars via your secrets engine's standard injection path (HashiCorp Vault Agent/Vault Injector, External Secrets Operator, AWS Secrets Manager/Parameter Store CSI driver, etc.) rather than baking secrets into config files or the container image. Fail closed (refuse to start the affected check) if a configured env var is unset or empty, and never log its resolved value.
- **AWS auth for AWS-target checks** (`internal/awsauth`): the Admin "Add a service" form's ALB/NLB path lets the operator pick how the prober authenticates to the AWS API before it can call `DescribeTargetHealth` (ELBv2) or similar. Use the AWS SDK for Go v2 (`aws-sdk-go-v2`) credential providers per method: **Assume role** → `sts.AssumeRole` with the given role ARN (+ optional external ID), short-lived creds cached and refreshed before expiry; **Service account** → pull long-lived-but-rotated creds from Secrets Manager/Vault at the referenced path, never store raw keys in your own config store; **AWS SSO** → `aws sso login`-equivalent device/browser flow via the SDK's SSO token provider, caching the resulting short-lived session token (never the SSO password) and re-prompting only on expiry. Store which method + reference (role ARN / secret path / SSO profile) per service, not the credentials themselves. When "custom SSO login binary" is set, shell out to the configured binary/args (`os/exec`) instead of the SDK's built-in SSO flow and parse its stdout as JSON credentials — the same contract AWS CLI's `credential_process` uses — so orgs with an internal SSO wrapper (Okta-fronted or otherwise) can plug in without code changes; validate the binary path is on an allow-list before executing it.

The frontend (React/etc., built per the rest of this doc) then becomes a thin client over this API rather than owning any health logic itself.

## Assets
No images/icons used (Lucide icon set is the system default but this build uses text/emoji-free UI — a real implementation should add Lucide icons at stroke-width 2.75 for nav/actions per the design system, e.g. alert-triangle for incidents, activity for health, git-branch for rollouts).

## Files
- `Active-Active Health Checker.dc.html` — the full design reference (single-file prototype; template + inline logic).
- `screenshots/1-overview.png`, `2-services.png`, `3-failover-routing.png`, `4-rollouts.png`, `5-incidents.png` — reference screenshots of each tab.
