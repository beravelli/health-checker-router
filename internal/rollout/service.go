// Package rollout tracks per-service deploy progress across regions and gates
// promotion between them.
//
// The gate is the point of the package: a canary in one region is verified
// against the health checker before the other region is promoted, so a bad
// build cannot roll out to both sides of an active-active pair at once.
package rollout

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/beravelli/health-checker-router/internal/events"
	"github.com/beravelli/health-checker-router/internal/models"
)

// Store is the persistence this service needs.
type Store interface {
	ListRollouts(ctx context.Context) ([]models.Rollout, error)
	SetRolloutPaused(ctx context.Context, id string, paused bool) error
	RollbackRollout(ctx context.Context, id string) error
}

// HealthProvider reports a service's current status in a region.
type HealthProvider interface {
	Status(serviceID, regionID string) (models.Status, bool)
}

// Service manages rollouts.
type Service struct {
	store  Store
	health HealthProvider
	hub    *events.Hub
	log    *slog.Logger
}

// New constructs a Service.
func New(s Store, health HealthProvider, hub *events.Hub, log *slog.Logger) *Service {
	return &Service{store: s, health: health, hub: hub, log: log}
}

// List returns all rollouts.
func (s *Service) List(ctx context.Context) ([]models.Rollout, error) {
	return s.store.ListRollouts(ctx)
}

// Pause halts a rollout.
func (s *Service) Pause(ctx context.Context, id, actor string) error {
	if err := s.store.SetRolloutPaused(ctx, id, true); err != nil {
		return err
	}
	s.log.Info("rollout paused", "rollout", id, "actor", actor)
	s.publish(ctx)
	return nil
}

// Resume restarts a paused rollout, but only if the canary region is healthy.
//
// Resuming into a region that is already unhealthy is how a partial outage
// becomes a total one, so the check is a hard gate rather than a warning.
func (s *Service) Resume(ctx context.Context, id, actor string) error {
	rollouts, err := s.store.ListRollouts(ctx)
	if err != nil {
		return err
	}

	var target *models.Rollout
	for i := range rollouts {
		if rollouts[i].ID == id {
			target = &rollouts[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("rollout %s not found", id)
	}

	if err := s.canPromote(*target); err != nil {
		return err
	}
	if err := s.store.SetRolloutPaused(ctx, id, false); err != nil {
		return err
	}
	s.log.Info("rollout resumed", "rollout", id, "actor", actor)
	s.publish(ctx)
	return nil
}

// Rollback zeroes the trailing region's progress and pauses the rollout.
func (s *Service) Rollback(ctx context.Context, id, actor string) error {
	if err := s.store.RollbackRollout(ctx, id); err != nil {
		return err
	}
	s.log.Warn("rollout rolled back", "rollout", id, "actor", actor)
	s.publish(ctx)
	return nil
}

// canPromote reports whether a rollout may advance, based on the health of the
// service in the region that is already further along.
func (s *Service) canPromote(r models.Rollout) error {
	if r.ServiceID == "" || s.health == nil {
		return nil
	}

	// The leading region is the canary; its health is what qualifies the build.
	canaryRegion := "us-east-1"
	if r.WestPct > r.EastPct {
		canaryRegion = "us-west-2"
	}

	status, ok := s.health.Status(r.ServiceID, canaryRegion)
	if !ok {
		// Never probed. Allow the operator to proceed rather than blocking on
		// missing data, but say so.
		s.log.Warn("resuming rollout without canary health data",
			"rollout", r.ID, "region", canaryRegion)
		return nil
	}

	switch status {
	case models.StatusCritical:
		return fmt.Errorf("cannot resume: %s is critical in the canary region %s", r.Service, canaryRegion)
	case models.StatusUnknown:
		return fmt.Errorf("cannot resume: health of %s in %s is inconclusive — verify before promoting",
			r.Service, canaryRegion)
	}
	return nil
}

func (s *Service) publish(ctx context.Context) {
	list, err := s.store.ListRollouts(ctx)
	if err != nil {
		s.log.Error("publish rollouts", "error", err)
		return
	}
	s.hub.Publish(events.TypeRollout, list)
}
