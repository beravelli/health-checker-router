// Package incident records incidents and pages on-call.
//
// Paging is deduplicated by title within a suppression window, because the
// evaluation loop runs every few seconds and an unresolved condition would
// otherwise page continuously.
package incident

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/beravelli/health-checker-router/internal/events"
	"github.com/beravelli/health-checker-router/internal/models"
)

// Store is the persistence this service needs.
type Store interface {
	CreateIncident(ctx context.Context, i models.Incident) (models.Incident, error)
	ListIncidents(ctx context.Context, severity string, limit int) ([]models.Incident, error)
	RecentIncidentExists(ctx context.Context, title string, within time.Duration) (bool, error)
}

// Notifier delivers a page to an external on-call system.
type Notifier interface {
	Notify(ctx context.Context, i models.Incident) error
}

// Service records incidents and fans them out.
type Service struct {
	store     Store
	notifiers []Notifier
	hub       *events.Hub
	log       *slog.Logger
	// suppress is how long a repeat of the same title is swallowed.
	suppress time.Duration
}

// New constructs a Service with a 15 minute page-suppression window.
func New(s Store, hub *events.Hub, log *slog.Logger, notifiers ...Notifier) *Service {
	return &Service{
		store:     s,
		notifiers: notifiers,
		hub:       hub,
		log:       log,
		suppress:  15 * time.Minute,
	}
}

// List returns incidents, optionally filtered by severity.
func (s *Service) List(ctx context.Context, severity string, limit int) ([]models.Incident, error) {
	return s.store.ListIncidents(ctx, severity, limit)
}

// Page records an incident and notifies on-call, unless an identical incident
// was recorded inside the suppression window.
//
// It satisfies failopen.Pager.
func (s *Service) Page(ctx context.Context, severity models.Severity, region, title, description string) error {
	// Resolutions are always recorded — suppressing them would leave the
	// timeline showing an open incident that has actually cleared.
	if severity != models.SeverityResolved {
		recent, err := s.store.RecentIncidentExists(ctx, title, s.suppress)
		if err != nil {
			return fmt.Errorf("check recent incidents: %w", err)
		}
		if recent {
			s.log.Debug("page suppressed — duplicate within window", "title", title)
			return nil
		}
	}

	created, err := s.store.CreateIncident(ctx, models.Incident{
		Severity:    severity,
		RegionID:    region,
		Title:       title,
		Description: description,
	})
	if err != nil {
		return fmt.Errorf("record incident: %w", err)
	}

	s.hub.Publish(events.TypeIncident, created)
	s.log.Info("incident recorded", "severity", severity, "region", region, "title", title)

	// Notifier failures must not fail the caller: the incident is already
	// durably recorded, and a paging outage should not cascade into the
	// health checker itself.
	for _, n := range s.notifiers {
		if err := n.Notify(ctx, created); err != nil {
			s.log.Error("notify on-call failed", "title", title, "error", err)
		}
	}
	return nil
}

// LogNotifier writes pages to the log. It is the default when no external
// pager is wired up, so a deployment without PagerDuty still leaves a trail.
type LogNotifier struct{ Log *slog.Logger }

// Notify logs the incident.
func (n LogNotifier) Notify(_ context.Context, i models.Incident) error {
	n.Log.Warn("PAGE",
		"severity", i.Severity, "region", i.RegionID,
		"title", i.Title, "description", i.Description)
	return nil
}

// WebhookNotifier posts incidents to an external endpoint (PagerDuty Events
// API, Opsgenie, a Slack webhook, and so on).
type WebhookNotifier struct {
	URL    string
	Client interface {
		Post(ctx context.Context, url string, body any) error
	}
}

// Notify posts the incident to the configured webhook.
func (n WebhookNotifier) Notify(ctx context.Context, i models.Incident) error {
	if n.URL == "" || n.Client == nil {
		return nil
	}
	return n.Client.Post(ctx, n.URL, map[string]any{
		"severity":    i.Severity,
		"region":      i.RegionID,
		"title":       i.Title,
		"description": i.Description,
		"occurredAt":  i.OccurredAt,
	})
}
