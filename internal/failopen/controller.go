// Package failopen implements the quorum and fail-open policy.
//
// The distinction this package exists to preserve: "confirmed unhealthy" and
// "we cannot tell" are different states and must produce different actions.
// A confirmed failure should move traffic. An inconclusive signal should not —
// it should hold the last-known-good weights, page on-call, and wait, because
// guessing risks pulling a region that is actually serving fine.
package failopen

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/beravelli/health-checker-router/internal/events"
	"github.com/beravelli/health-checker-router/internal/models"
)

// Policy is the fail-open configuration surfaced in the UI.
type Policy struct {
	// QuorumRequired probes of QuorumTotal must return a conclusive result for
	// a region's health signal to be trusted.
	QuorumRequired int `json:"quorumRequired"`
	QuorumTotal    int `json:"quorumTotal"`
	// Hold is how long last-known-good weights may be held before the
	// situation is escalated as a confirmed problem.
	Hold time.Duration `json:"holdSeconds"`
}

// Store is the persistence this controller needs.
type Store interface {
	ActiveFailOpen(ctx context.Context) (*models.FailOpenEvent, error)
	OpenFailOpen(ctx context.Context, reason string) (*models.FailOpenEvent, error)
	ResolveFailOpen(ctx context.Context) error
}

// Pager is notified when a fail-open window opens or exceeds its hold.
type Pager interface {
	Page(ctx context.Context, severity models.Severity, region, title, description string) error
}

// UnknownRatioProvider reports the share of a region's targets whose signal is
// currently inconclusive.
type UnknownRatioProvider interface {
	UnknownRatio(regionID string) float64
}

// Decision is the outcome of one evaluation pass.
type Decision struct {
	// Trusted regions have a usable health signal; their computed weights may
	// be applied.
	Trusted map[string]bool `json:"trusted"`
	// FailOpen is true when at least one region's signal is inconclusive and
	// the last-known-good weights are being held.
	FailOpen bool `json:"failOpen"`
	// HoldExpired is true when the fail-open window has run past Policy.Hold,
	// meaning the inconclusive state has itself become the incident.
	HoldExpired bool      `json:"holdExpired"`
	Reason      string    `json:"reason,omitempty"`
	Since       time.Time `json:"since,omitempty"`
}

// Controller evaluates quorum and owns the fail-open window.
type Controller struct {
	policy Policy
	store  Store
	pager  Pager
	hub    *events.Hub
	log    *slog.Logger

	mu sync.Mutex
	// pagedForHold ensures the hold-expiry page fires once per window, not on
	// every evaluation tick.
	pagedForHold bool
}

// New constructs a Controller, filling in the 2-of-3 default policy.
//
// QuorumTotal must end up strictly greater than QuorumRequired. Equal values
// yield zero unknown tolerance, which would distrust a region on its first
// inconclusive probe and pin the system into a permanent fail-open hold — the
// opposite of the intent. config.Load rejects that combination outright; here
// it is corrected, since New also serves callers that build a Policy directly.
func New(policy Policy, s Store, pager Pager, hub *events.Hub, log *slog.Logger) *Controller {
	if policy.QuorumRequired < 1 {
		policy.QuorumRequired = 2
	}
	if policy.QuorumTotal <= policy.QuorumRequired {
		policy.QuorumTotal = policy.QuorumRequired + 1
	}
	if policy.Hold <= 0 {
		policy.Hold = 10 * time.Minute
	}
	return &Controller{policy: policy, store: s, pager: pager, hub: hub, log: log}
}

// Policy returns the active policy, for the Failover & routing tab.
func (c *Controller) Policy() Policy { return c.policy }

// unknownTolerance is the fraction of a region's targets that may be
// inconclusive while the region's signal is still trustworthy.
//
// Derived directly from the quorum rule. QuorumRequired of QuorumTotal probes
// must be conclusive, so the conclusive share must be at least
// QuorumRequired/QuorumTotal, and the inconclusive share may be at most
// (QuorumTotal-QuorumRequired)/QuorumTotal. With the 2-of-3 default that is one
// third.
//
// This previously carried a +1 in the numerator, which returned two thirds for
// the 2-of-3 default — double the documented tolerance. The effect was that a
// region kept driving traffic weights until two of every three probes had gone
// dark, which is precisely the blindness this package exists to refuse to act
// on.
func (c *Controller) unknownTolerance() float64 {
	return float64(c.policy.QuorumTotal-c.policy.QuorumRequired) / float64(c.policy.QuorumTotal)
}

// Evaluate decides, per region, whether the health signal can be trusted, and
// opens or closes the fail-open window accordingly.
func (c *Controller) Evaluate(ctx context.Context, regions []models.Region, ratios UnknownRatioProvider) (Decision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	d := Decision{Trusted: map[string]bool{}}
	tolerance := c.unknownTolerance()

	var untrusted []string
	for _, r := range regions {
		ratio := ratios.UnknownRatio(r.ID)
		// <= not <: at exactly the tolerance the quorum rule is still satisfied.
		trusted := ratio <= tolerance
		d.Trusted[r.ID] = trusted
		if !trusted {
			untrusted = append(untrusted, fmt.Sprintf("%s (%.0f%% of probes inconclusive)", r.ID, ratio*100))
		}
	}

	active, err := c.store.ActiveFailOpen(ctx)
	if err != nil {
		return d, fmt.Errorf("read fail-open state: %w", err)
	}

	// No region is untrustworthy: close any open window.
	if len(untrusted) == 0 {
		if active != nil {
			if err := c.store.ResolveFailOpen(ctx); err != nil {
				return d, fmt.Errorf("resolve fail-open: %w", err)
			}
			c.pagedForHold = false
			c.log.Info("fail-open window closed", "held", time.Since(active.TriggeredAt))
			c.hub.Publish(events.TypeFailOpen, map[string]any{
				"active": false,
				"heldMs": time.Since(active.TriggeredAt).Milliseconds(),
			})
			if err := c.pager.Page(ctx, models.SeverityResolved, "",
				"Fail-open window closed",
				fmt.Sprintf("Probe quorum recovered after %s. Health-based weighting resumed.",
					time.Since(active.TriggeredAt).Round(time.Second)),
			); err != nil {
				c.log.Error("page on fail-open close", "error", err)
			}
		}
		return d, nil
	}

	reason := "Probe quorum inconclusive for " + joinComma(untrusted)
	d.FailOpen = true
	d.Reason = reason

	if active == nil {
		opened, err := c.store.OpenFailOpen(ctx, reason)
		if err != nil {
			return d, fmt.Errorf("open fail-open: %w", err)
		}
		c.pagedForHold = false
		d.Since = opened.TriggeredAt
		c.log.Warn("fail-open activated — holding last-known-good weights", "reason", reason)
		c.hub.Publish(events.TypeFailOpen, map[string]any{
			"active": true, "reason": reason, "since": opened.TriggeredAt,
		})
		// Page immediately. An inconclusive signal is exactly the case where a
		// human needs to look, because the system has deliberately chosen not
		// to act on its own.
		if err := c.pager.Page(ctx, models.SeverityWarning, "",
			"Fail-open activated",
			reason+". LB is holding last-known-good weights; verify before trusting health signals.",
		); err != nil {
			c.log.Error("page on fail-open open", "error", err)
		}
		return d, nil
	}

	d.Since = active.TriggeredAt
	if held := time.Since(active.TriggeredAt); held > c.policy.Hold {
		d.HoldExpired = true
		if !c.pagedForHold {
			c.pagedForHold = true
			c.log.Error("fail-open hold exceeded", "held", held, "limit", c.policy.Hold)
			if err := c.pager.Page(ctx, models.SeverityCritical, "",
				"Fail-open hold exceeded",
				fmt.Sprintf("Health signal has been inconclusive for %s, past the %s hold. "+
					"Manual intervention required — the checker will not fail over on an unverified signal.",
					held.Round(time.Second), c.policy.Hold),
			); err != nil {
				c.log.Error("page on hold expiry", "error", err)
			}
		}
	}
	return d, nil
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
