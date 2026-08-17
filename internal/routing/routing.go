// Package routing defines the common interface every external traffic manager
// adapter implements, plus the registry that resolves the configured provider.
//
// Weight changes go through the adapter rather than being hardcoded to one
// vendor, and deliberately *not* through DNS: DNS TTL caching means a record
// change can take minutes to reach clients, which is why the DNS-TTL edge case
// exists. Each adapter pushes to the layer that takes effect immediately for
// its provider, and treats DNS as a backstop only.
package routing

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/beravelli/health-checker-router/internal/models"
)

// Weights maps region id to a 0..100 traffic weight.
type Weights map[string]int

// Validate checks that weights are in range and sum to 100.
func (w Weights) Validate() error {
	if len(w) == 0 {
		return fmt.Errorf("no weights supplied")
	}
	sum := 0
	for region, v := range w {
		if v < 0 || v > 100 {
			return fmt.Errorf("weight for %s is %d, must be 0..100", region, v)
		}
		sum += v
	}
	if sum != 100 {
		return fmt.Errorf("weights sum to %d, must sum to 100", sum)
	}
	return nil
}

// Regions returns the region ids in stable order.
func (w Weights) Regions() []string {
	out := make([]string, 0, len(w))
	for r := range w {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// ProviderStatus is what an adapter reports about the live routing state.
type ProviderStatus struct {
	Provider  models.RoutingProvider `json:"provider"`
	Reachable bool                   `json:"reachable"`
	Weights   Weights                `json:"weights,omitempty"`
	Detail    string                 `json:"detail,omitempty"`
}

// Provider is the contract every traffic manager adapter satisfies.
type Provider interface {
	// Name identifies the provider.
	Name() models.RoutingProvider

	// SetWeight applies a traffic split across regions.
	SetWeight(ctx context.Context, w Weights) error

	// Failover sends all traffic to the named region and pulls the others out
	// of rotation. Implementations should use their provider's fastest
	// mechanism, not a DNS record change.
	Failover(ctx context.Context, toRegion string) error

	// Status reports the live routing configuration as the provider sees it,
	// which may differ from what this service last pushed.
	Status(ctx context.Context) (ProviderStatus, error)
}

// Factory builds a Provider from its stored configuration.
//
// Credentials are never passed in: the endpoint carries an AuthRef naming an
// environment variable or secret path, and the adapter resolves it at call
// time.
type Factory func(ctx context.Context, ep models.RoutingEndpoint) (Provider, error)

// Registry maps provider ids to factories.
type Registry struct {
	mu        sync.RWMutex
	factories map[models.RoutingProvider]Factory
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{factories: map[models.RoutingProvider]Factory{}}
}

// Register adds a factory for a provider.
func (r *Registry) Register(name models.RoutingProvider, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = f
}

// Build resolves the adapter for an endpoint's provider.
func (r *Registry) Build(ctx context.Context, ep models.RoutingEndpoint) (Provider, error) {
	r.mu.RLock()
	f, ok := r.factories[ep.Provider]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no adapter registered for routing provider %q", ep.Provider)
	}
	if !ep.Enabled {
		return nil, fmt.Errorf("routing provider %q is not enabled", ep.Provider)
	}
	return f(ctx, ep)
}

// Registered lists the providers that have adapters, in stable order.
func (r *Registry) Registered() []models.RoutingProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]models.RoutingProvider, 0, len(r.factories))
	for name := range r.factories {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ConfigString reads a string field from an endpoint's config, erroring when
// a required field is missing so misconfiguration surfaces at call time rather
// than as a confusing provider-side rejection.
func ConfigString(ep models.RoutingEndpoint, key string, required bool) (string, error) {
	v, _ := ep.Config[key].(string)
	if v == "" && required {
		return "", fmt.Errorf("routing provider %s: config field %q is required", ep.Provider, key)
	}
	return v, nil
}
