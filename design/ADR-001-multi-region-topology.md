# ADR-001: Multi-region topology and routing control plane

**Status:** Accepted (partially implemented — see [Implementation status](#implementation-status))
**Date:** 2026-08-18
**Context:** Deploying the health checker alongside the services it monitors in
`us-east-1` and `us-west-2`

---

## Context

The checker is a control plane for an active-active deployment. The question is
where *it* runs, and whether it should itself be active-active to survive a
region failure or an inter-region partition.

Three requirements pull in different directions:

1. **Signal usefulness** wants probes wherever they answer the routing question
   best. See decision 1 — this turned out not to mean "in-region".
2. **Availability** wants the checker to survive losing any one region,
   including a region it monitors.
3. **Correctness** wants exactly one entity deciding where traffic goes.

Requirements 2 and 3 are the ones that conflict, and the conflict is not
resolvable by making the checker active-active. That is the crux of this ADR.

> **Revised 2026-08-18.** An earlier draft argued for in-region probes on
> latency-accuracy grounds. That was wrong, or at least it was answering a
> question the checker does not ask. Latency measurement is a separate concern
> served by purpose-built per-region agents; it does not need to ride on the
> health checker. Decision 1 below is the corrected version.

---

## Decision

### 1. Probes need vantage *diversity*, not co-location

The checker answers one question: **can traffic reach this region and get
served**. It does not answer "how fast is this region internally" — that is a
performance question, served better by dedicated per-region latency agents that
have nothing to do with failover.

Once latency is out of scope, the case for in-region probes mostly evaporates,
and probing from outside is arguably *more* correct for the routing decision:

- A region whose services are healthy from inside their own VPC but unreachable
  from where traffic originates **must** be evacuated. An in-region probe
  reports healthy and misses the outage entirely — a false negative on a real
  failure.
- Externally-originating probes traverse the same edge, load balancers and
  security groups that customer traffic does. That is the path whose health
  determines whether traffic should be sent.

What genuinely matters is **more than one vantage point**, so the system can
distinguish "the target is down" from "my path to the target is down". One
observer cannot tell those apart no matter where it sits. Two or three can.

So: decisions run in exactly one place that is not a monitored region — a
controller inside us-east-1 evaluating us-east-1 degrades along with it and
disappears when needed. Probes run from at least two vantage points, at least
one of which is outside every monitored region. Whether a vantage point happens
to sit inside a monitored region is an availability and reachability question,
not an accuracy one.

**Consequence for configuration:** `degradedAboveMs` turns probe latency into a
`degraded` status, which scores 0.5 instead of 1.0 and sheds traffic. A
threshold calibrated for in-region probing will fire permanently once the probe
runs cross-region, because every measurement now carries inter-region RTT — a
standing false degradation that quietly moves traffic. Either raise the
threshold to suit the new vantage point or unset it and let dedicated latency
agents own that signal. Unset is the better default here.

### 2. The decision plane is single-writer, and deliberately not active-active

Active-active is right for the services. It is wrong for the controller.

Consider an inter-region partition with both regions perfectly healthy. Two
active controllers:

- East observes west go unknown; west observes east go unknown.
- Each concludes it is fine and the other is dark.
- Each writes weights favouring itself.
- The routing provider accepts both. Last writer wins, then loses, then wins.

Traffic oscillates during an event where nothing was broken — worse than any
single wrong decision. This is classic split-brain, and for traffic routing the
blast radius is total.

The existing fail-open design already handles this correctly **provided there is
one writer**: both regions go unknown, quorum fails, last-known-good weights
hold, on-call is paged. Adding a second writer destroys that property.

What we want is not active-active but **single-writer with quorum-based
failover**: one decider at a time, elected such that losing any one region
elects a new one.

### 3. Leadership stays a Postgres advisory lock, and the database moves

`internal/leader` already elects a single writer through `pg_try_advisory_lock`.
The mechanism is sound; its placement is the problem.

If Postgres lives in a monitored region, losing that region loses the database,
the lock, leadership, and therefore any ability to fail over — the system dies
at precisely the moment it exists to act. Aurora Global Database does not fix
this cleanly: the secondary is read-only and advisory locks need a writer, so
recovery waits on promotion.

**Decision:** the control plane — decider and database — lives in a third region
that hosts nothing else. Its failure is uncorrelated with either monitored
region, and it is small: one deployment and one database, not a copy of the
stack.

### 4. Evacuation goes through ARC, not through record writes

`ChangeResourceRecordSets` and every other Route 53 record and health-check
mutation is a **control-plane** operation hosted in **us-east-1**. AWS names
"update the weight of a weighted record to perform failover" as the canonical
non-statically-stable pattern for exactly this reason.

The consequence for us is sharp: **if us-east-1 is degraded, the call that would
evacuate us-east-1 is the call that fails.** The `awsnative` adapter has this
dependency today.

Amazon Application Recovery Controller routing controls invert it. The routing
control data plane is a cluster of five endpoints across five geographically
isolated Regions on a quorum model, functional with up to two endpoints
unavailable, with a 100% availability SLA. Flipping a control changes the health
check state that Route 53 already evaluates in its data plane — no control-plane
call.

**Decision:** add an ARC adapter and use it for evacuation. Keep the weighted
adapters for proportional shifts.

#### Why both, rather than replacing one with the other

Routing controls are **On/Off**. They cannot express 70/30. Pretending otherwise
would be a bad abstraction in a system whose entire premise is proportional
weighting. The two mechanisms answer different questions:

| Question | Mechanism | Why |
| --- | --- | --- |
| *How much* traffic should this region take? | Route 53 weights / Envoy edge | Proportional; a degraded cache should shed some traffic, not all |
| *Is this region in rotation at all?* | ARC routing control | Binary, but works during a regional control-plane event |

Degradation is the common case and wants proportion. Evacuation is the rare,
high-stakes case and wants to work when AWS is having a bad day.

If you keep record writes in the path, enable [Route 53 Accelerated
Recovery](https://aws.amazon.com/blogs/networking-and-content-delivery/announcing-amazon-route-53-accelerated-recovery-for-managing-public-dns-records/),
which fails control-plane operations over to us-west-2 within 60 minutes. Better
than nothing; not a failover budget.

### 5. Alert delivery must not depend on the monitored regions

The dead man's switch (`hcr_last_evaluation_timestamp_seconds`) is only
meaningful if the alert can leave. Alertmanager, and its notification path, run
outside the monitored regions — otherwise the outage that silences the checker
silences the alert about it.

---

## Target topology

Drawn out, with the per-failure behaviour matrix, in
[deployment-architecture.md](deployment-architecture.md).

```
us-east-1 (monitored)          us-west-2 (monitored)
  services                        services
  latency agent (separate)        latency agent (separate)
         ^                             ^
          \      probed from          /
           \     >= 2 vantages       /
          us-west-1 (control plane, hosts nothing else)
            checker         (leader-elected, single writer)
            Postgres        (advisory lock lives here)
            Prometheus + Alertmanager
                    |
                    v
          ARC routing controls (5-endpoint data plane)
          Route 53 weights / Envoy edge (proportional)
```

The decider owns every write. Latency agents are a separate system feeding
dashboards, not the routing decision.

Vantage points can be a second checker instance, a witness target, or an
external synthetic. What matters is that they do not all share one network path
— not that they sit inside the monitored regions.

---

## Consequences

### Good

- Survives losing either monitored region: the decider is elsewhere, and losing
  a monitored region does not remove an observer of it.
- Survives an inter-region partition without split-brain: one writer, and the
  fail-open controller holds weights when the signal is inconclusive.
- Survives a us-east-1 control-plane event, because evacuation no longer needs
  the Route 53 API.
- Probes traverse the same edge path customer traffic does, so a region that is
  internally healthy but externally unreachable is still caught.

### Bad

- A third region to operate, pay for, and deploy to.
- The control plane is now a distinct failure domain of its own. Losing it means
  no automatic failover — though the fail-open design already treats "cannot
  decide" as "hold and page", so the failure mode is inaction rather than a
  wrong action. That is the correct trade, but it must be understood: **the
  system is not self-healing during a control-plane outage.**
- Probing from one place cannot distinguish a dead target from a dead path. A
  second vantage point is required, though that is far less work than splitting
  the prober out of the decider.
- `degradedAboveMs` thresholds calibrated in-region become wrong. See decision 1.
- Two routing mechanisms to keep coherent. `Status()` from ARC and from a
  weighted provider will legitimately disagree, and operators need to know that
  is information rather than drift.

### Rejected alternatives

**Active-active controllers, one per region.** Split-brain, as above.

**Controller in one monitored region with a standby in the other.** Better than
nothing, but the standby's view of the failed region is exactly the view that is
compromised, and the Postgres placement problem is unchanged.

**Kubernetes leases instead of advisory locks.** The Kubernetes API is not a
consistency oracle we already depend on, and a leader that cannot reach Postgres
cannot function regardless.

**ARC only, dropping weighted routing.** Loses proportional response, which is
design principle 1 and the reason this system exists rather than a binary health
check.

**Route 53 ARC readiness checks instead of our own probing.** Readiness checks
are about capacity and configuration drift, not composite service health, and
they do not express the weighted scoring model.

---

## Implementation status

Done:

- `internal/leader` — single-writer election over a Postgres advisory lock
- `internal/routing/arc` — ARC adapter, with sequential failover across all five
  cluster endpoints, atomic batched failover, and safety rules never overridden
  by default
- `internal/obs` — heartbeat and the alerts that fire on silence

Not done:

- **A second vantage point.** Today there is exactly one observer, so "the
  target is down" and "our path to the target is down" are indistinguishable.
  This is now the largest remaining gap in the topology. A witness probe is the
  cheap version; a second checker instance elsewhere is the thorough one.
- Deploying the control plane to a third region (currently a chart value away,
  but untested)
- Guidance or validation on `degradedAboveMs` when probing cross-region

Running the checker from a third region is the correct configuration today. The
earlier concern — that this degrades signal accuracy — was about latency, which
this system no longer claims to measure.

---

## References

- [Global services — AWS Fault Isolation Boundaries](https://docs.aws.amazon.com/whitepapers/latest/aws-fault-isolation-boundaries/global-services.html)
  — Route 53 control plane is hosted in us-east-1
- [Data and control planes for routing control — ARC](https://docs.aws.amazon.com/r53recovery/latest/dg/data-and-control-planes.html)
  — five-endpoint cluster, quorum model, retry each endpoint in sequence
- [AWS Region availability for routing control](https://docs.aws.amazon.com/r53recovery/latest/dg/introduction-regions-routing.html)
- [Announcing Route 53 Accelerated Recovery](https://aws.amazon.com/blogs/networking-and-content-delivery/announcing-amazon-route-53-accelerated-recovery-for-managing-public-dns-records/)
- [Reducing control plane dependencies in us-east-1](https://repost.aws/articles/ARe7BZRCUhRlap9b7yFSagLw/reducing-control-plane-dependencies-in-us-east-1-a-practical-guide-for-aws-customers)
