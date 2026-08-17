// Package lb owns the authoritative load-balancer weight state and is the only
// place weights are allowed to change.
//
// Two rules shape this package:
//
//  1. Weights derive from the composite health score, not from a binary
//     up/down. A region at 0.8 health loses some traffic; it is not evicted.
//  2. When the fail-open controller says a region's signal is inconclusive,
//     automatic reweighting is suppressed entirely and the last-known-good
//     weights stand. Manual operator action still goes through.
package lb

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/beravelli/health-checker-router/internal/events"
	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/routing"
)

// Store is the persistence this controller needs.
type Store interface {
	GetWeights(ctx context.Context) ([]models.LBWeight, error)
	SetWeights(ctx context.Context, weights map[string]int, automatic bool) error
	ActiveRoutingEndpoint(ctx context.Context) (models.RoutingEndpoint, error)
}

// minRegionWeight keeps a degraded-but-serving region from being starved
// entirely by automatic reweighting. Full eviction requires either a critical
// score or an explicit manual failover.
const minRegionWeight = 5

// Controller computes, persists, and pushes LB weights.
type Controller struct {
	store    Store
	registry *routing.Registry
	hub      *events.Hub
	log      *slog.Logger

	mu sync.Mutex
}

// New constructs a Controller.
func New(s Store, reg *routing.Registry, hub *events.Hub, log *slog.Logger) *Controller {
	return &Controller{store: s, registry: reg, hub: hub, log: log}
}

// Current returns the persisted weights.
func (c *Controller) Current(ctx context.Context) (routing.Weights, bool, error) {
	rows, err := c.store.GetWeights(ctx)
	if err != nil {
		return nil, false, err
	}
	w := routing.Weights{}
	automatic := true
	for _, r := range rows {
		w[r.RegionID] = r.Weight
		if !r.IsAutomatic {
			automatic = false
		}
	}
	return w, automatic, nil
}

// Reconcile recomputes weights from region health scores and pushes them.
//
// trusted marks which regions have a usable signal. If any region is
// untrusted, the whole reconcile is skipped: a split computed from partially
// unknown inputs is worse than leaving the last good split in place.
func (c *Controller) Reconcile(ctx context.Context, scores map[string]float64, trusted map[string]bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for region, ok := range trusted {
		if !ok {
			c.log.Info("skipping automatic reweight — signal inconclusive", "region", region)
			return nil
		}
	}

	_, automatic, err := c.Current(ctx)
	if err != nil {
		return fmt.Errorf("read current weights: %w", err)
	}
	if !automatic {
		// An operator has taken manual control. Respect it until they reset.
		return nil
	}

	target := WeightsFromScores(scores)
	if err := target.Validate(); err != nil {
		return fmt.Errorf("computed weights invalid: %w", err)
	}

	current, _, err := c.Current(ctx)
	if err != nil {
		return err
	}
	if equal(current, target) {
		return nil
	}

	return c.apply(ctx, target, true, "automatic health-based reweight")
}

// SetManual applies an operator-chosen split and disables automatic control.
func (c *Controller) SetManual(ctx context.Context, w routing.Weights, actor string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := w.Validate(); err != nil {
		return err
	}
	return c.apply(ctx, w, false, "manual weight change by "+actor)
}

// ResetToAutomatic hands control back to health-based weighting.
//
// It only flips the flag; the next Reconcile computes the real split. That
// avoids a jump to an arbitrary default before health data is consulted.
func (c *Controller) ResetToAutomatic(ctx context.Context, actor string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	current, _, err := c.Current(ctx)
	if err != nil {
		return err
	}
	if err := c.store.SetWeights(ctx, current, true); err != nil {
		return fmt.Errorf("reset to automatic: %w", err)
	}
	c.log.Info("weights returned to automatic control", "actor", actor)
	c.hub.Publish(events.TypeWeights, map[string]any{
		"weights": current, "automatic": true, "reason": "reset to automatic by " + actor,
	})
	return nil
}

// Failover routes all traffic to one region and pulls the others out.
//
// This is the manual, confirmed-incident path. It goes straight to the
// provider rather than waiting for a reconcile tick.
func (c *Controller) Failover(ctx context.Context, toRegion, actor string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	current, _, err := c.Current(ctx)
	if err != nil {
		return err
	}
	if _, ok := current[toRegion]; !ok {
		return fmt.Errorf("unknown region %q", toRegion)
	}

	w := routing.Weights{}
	for region := range current {
		w[region] = 0
	}
	w[toRegion] = 100

	if err := c.apply(ctx, w, false, fmt.Sprintf("manual failover to %s by %s", toRegion, actor)); err != nil {
		return err
	}
	c.log.Warn("manual failover executed", "region", toRegion, "actor", actor)
	return nil
}

// apply persists weights and pushes them to the active routing provider.
//
// Persistence happens first so the recorded intent survives a provider
// failure; a push error is returned but does not roll back the stored state,
// because the operator needs to see what was intended in order to retry.
func (c *Controller) apply(ctx context.Context, w routing.Weights, automatic bool, reason string) error {
	if err := c.store.SetWeights(ctx, w, automatic); err != nil {
		return fmt.Errorf("persist weights: %w", err)
	}

	c.hub.Publish(events.TypeWeights, map[string]any{
		"weights": w, "automatic": automatic, "reason": reason, "at": time.Now().UTC(),
	})

	ep, err := c.store.ActiveRoutingEndpoint(ctx)
	if err != nil {
		// No provider configured yet: weights are recorded and the Envoy edge
		// still reads them, but nothing external is pushed.
		c.log.Warn("weights stored but not pushed — no routing provider enabled",
			"reason", reason, "error", err)
		return nil
	}

	provider, err := c.registry.Build(ctx, ep)
	if err != nil {
		return fmt.Errorf("build routing provider %s: %w", ep.Provider, err)
	}

	pushCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := provider.SetWeight(pushCtx, w); err != nil {
		return fmt.Errorf("push weights to %s: %w", ep.Provider, err)
	}

	c.log.Info("weights applied", "weights", w, "automatic", automatic,
		"provider", ep.Provider, "reason", reason)
	return nil
}

// WeightsFromScores converts health scores into an integer split summing to 100.
//
// Regions scoring at or near zero are dropped to zero; everything else keeps at
// least minRegionWeight so a recovering region still receives the trickle of
// real traffic needed to prove itself.
func WeightsFromScores(scores map[string]float64) routing.Weights {
	w := routing.Weights{}
	if len(scores) == 0 {
		return w
	}

	// A region scoring below the critical floor is excluded from the split.
	const criticalFloor = 0.05

	eligible := map[string]float64{}
	var total float64
	for region, s := range scores {
		if s < criticalFloor {
			w[region] = 0
			continue
		}
		if s < 0 {
			s = 0
		}
		eligible[region] = s
		total += s
	}

	// Everything is critical: hold an even split rather than sending traffic
	// nowhere. There is no good option here, and zero everywhere is the worst.
	if total == 0 {
		even := 100 / len(scores)
		for region := range scores {
			w[region] = even
		}
		return distributeRemainder(w, scores)
	}

	for region, s := range eligible {
		v := int(s / total * 100)
		if v < minRegionWeight {
			v = minRegionWeight
		}
		w[region] = v
	}
	return distributeRemainder(w, scores)
}

// distributeRemainder nudges weights so they sum to exactly 100.
func distributeRemainder(w routing.Weights, scores map[string]float64) routing.Weights {
	sum := 0
	for _, v := range w {
		sum += v
	}
	if sum == 100 || len(w) == 0 {
		return w
	}

	// Give (or take) the difference from the highest-scoring region, so
	// rounding never disadvantages the healthiest site.
	best, bestScore := "", -1.0
	for region := range w {
		if s := scores[region]; s > bestScore {
			best, bestScore = region, s
		}
	}
	if best == "" {
		return w
	}

	w[best] += 100 - sum
	if w[best] < 0 {
		w[best] = 0
	}
	if w[best] > 100 {
		w[best] = 100
	}
	return w
}

func equal(a, b routing.Weights) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
