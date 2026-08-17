// Package avigslb routes traffic by adjusting GSLB pool member ratios on an
// Avi (VMware NSX Advanced Load Balancer) controller.
//
// Avi's REST API is a read-modify-write against the whole GSLB service object,
// guarded by an ETag-equivalent, so this adapter fetches, mutates the member
// ratios, and PUTs the result back.
package avigslb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/beravelli/health-checker-router/internal/config"
	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/routing"
)

// apiVersion is the Avi controller API version this adapter targets.
const apiVersion = "22.1.3"

// Adapter implements routing.Provider against an Avi controller.
type Adapter struct {
	client        *http.Client
	controllerURL string
	gslbService   string
	tenant        string
	username      string
	// password is resolved from the secret reference at construction and held
	// only in memory.
	password string
	// regionMembers maps region id to the GSLB pool member IP or domain name.
	regionMembers map[string]string
}

// Factory builds an Adapter from stored configuration.
func Factory() routing.Factory {
	return func(ctx context.Context, ep models.RoutingEndpoint) (routing.Provider, error) {
		controller, err := routing.ConfigString(ep, "controllerUrl", true)
		if err != nil {
			return nil, err
		}
		service, err := routing.ConfigString(ep, "gslbServiceName", true)
		if err != nil {
			return nil, err
		}
		username, err := routing.ConfigString(ep, "username", true)
		if err != nil {
			return nil, err
		}
		tenant, _ := routing.ConfigString(ep, "tenant", false)
		if tenant == "" {
			tenant = "admin"
		}

		members, err := regionMembers(ep)
		if err != nil {
			return nil, err
		}

		// AuthRef names the env var holding the API token or password.
		password, err := config.Resolve(ep.AuthRef)
		if err != nil {
			return nil, fmt.Errorf("avi_gslb: resolve API credential: %w", err)
		}

		return &Adapter{
			client:        &http.Client{Timeout: 20 * time.Second},
			controllerURL: strings.TrimSuffix(controller, "/"),
			gslbService:   service,
			tenant:        tenant,
			username:      username,
			password:      password,
			regionMembers: members,
		}, nil
	}
}

// Name identifies the provider.
func (a *Adapter) Name() models.RoutingProvider { return models.ProviderAviGSLB }

// gslbService is the subset of the Avi GSLB service object this adapter needs.
// Unknown fields are preserved via rawObject so the PUT does not drop settings
// this adapter does not model.
type gslbService struct {
	raw    map[string]any
	uuid   string
	groups []map[string]any
}

// SetWeight maps region weights onto GSLB pool member ratios.
func (a *Adapter) SetWeight(ctx context.Context, w routing.Weights) error {
	if err := w.Validate(); err != nil {
		return fmt.Errorf("avi_gslb: %w", err)
	}

	svc, err := a.fetchService(ctx)
	if err != nil {
		return err
	}

	memberToRegion := map[string]string{}
	for region, member := range a.regionMembers {
		memberToRegion[member] = region
	}

	applied := 0
	for _, group := range svc.groups {
		members, ok := group["members"].([]any)
		if !ok {
			continue
		}
		for _, raw := range members {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			region, ok := memberToRegion[memberIdentity(m)]
			if !ok {
				continue
			}
			m["ratio"] = w[region]
			// Ratio 0 alone still leaves the member eligible in some Avi
			// versions; disabling it removes any ambiguity.
			m["enabled"] = w[region] > 0
			applied++
		}
	}
	if applied == 0 {
		return fmt.Errorf("avi_gslb: no GSLB pool members matched the configured regionMembers map")
	}

	return a.putService(ctx, svc)
}

// Failover sends all traffic to one region.
func (a *Adapter) Failover(ctx context.Context, toRegion string) error {
	if _, ok := a.regionMembers[toRegion]; !ok {
		return fmt.Errorf("avi_gslb: unknown failover target region %q", toRegion)
	}
	w := routing.Weights{}
	for region := range a.regionMembers {
		w[region] = 0
	}
	w[toRegion] = 100
	return a.SetWeight(ctx, w)
}

// Status reads back the live member ratios.
func (a *Adapter) Status(ctx context.Context) (routing.ProviderStatus, error) {
	st := routing.ProviderStatus{Provider: models.ProviderAviGSLB, Weights: routing.Weights{}}

	svc, err := a.fetchService(ctx)
	if err != nil {
		st.Detail = err.Error()
		return st, err
	}
	st.Reachable = true

	memberToRegion := map[string]string{}
	for region, member := range a.regionMembers {
		memberToRegion[member] = region
	}

	for _, group := range svc.groups {
		members, ok := group["members"].([]any)
		if !ok {
			continue
		}
		for _, raw := range members {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if region, ok := memberToRegion[memberIdentity(m)]; ok {
				if ratio, ok := m["ratio"].(float64); ok {
					st.Weights[region] = int(ratio)
				}
			}
		}
	}
	st.Detail = fmt.Sprintf("controller %s, GSLB service %s", a.controllerURL, a.gslbService)
	return st, nil
}

// fetchService loads the GSLB service object by name.
func (a *Adapter) fetchService(ctx context.Context) (*gslbService, error) {
	url := fmt.Sprintf("%s/api/gslbservice?name=%s", a.controllerURL, a.gslbService)
	body, err := a.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	var listing struct {
		Count   int              `json:"count"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("avi_gslb: decode GSLB service listing: %w", err)
	}
	if len(listing.Results) == 0 {
		return nil, fmt.Errorf("avi_gslb: GSLB service %q not found on controller", a.gslbService)
	}

	obj := listing.Results[0]
	uuid, _ := obj["uuid"].(string)
	if uuid == "" {
		return nil, fmt.Errorf("avi_gslb: GSLB service %q has no uuid", a.gslbService)
	}

	svc := &gslbService{raw: obj, uuid: uuid}
	if groups, ok := obj["groups"].([]any); ok {
		for _, g := range groups {
			if m, ok := g.(map[string]any); ok {
				svc.groups = append(svc.groups, m)
			}
		}
	}
	if len(svc.groups) == 0 {
		return nil, fmt.Errorf("avi_gslb: GSLB service %q has no pool groups", a.gslbService)
	}
	return svc, nil
}

// putService writes the mutated object back.
func (a *Adapter) putService(ctx context.Context, svc *gslbService) error {
	payload, err := json.Marshal(svc.raw)
	if err != nil {
		return fmt.Errorf("avi_gslb: encode GSLB service: %w", err)
	}
	url := fmt.Sprintf("%s/api/gslbservice/%s", a.controllerURL, svc.uuid)
	if _, err := a.do(ctx, http.MethodPut, url, payload); err != nil {
		return err
	}
	return nil
}

// do issues an authenticated request against the controller.
func (a *Adapter) do(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("avi_gslb: build request: %w", err)
	}
	req.SetBasicAuth(a.username, a.password)
	req.Header.Set("X-Avi-Version", apiVersion)
	req.Header.Set("X-Avi-Tenant", a.tenant)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("avi_gslb: %s %s: %w", method, redactURL(url), err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := new(bytes.Buffer)
	if _, err := out.ReadFrom(resp.Body); err != nil {
		return nil, fmt.Errorf("avi_gslb: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("avi_gslb: %s %s returned %d: %s",
			method, redactURL(url), resp.StatusCode, truncate(out.String(), 300))
	}
	return out.Bytes(), nil
}

// memberIdentity returns the value a GSLB member is keyed by, preferring an
// explicit IP and falling back to its FQDN.
func memberIdentity(m map[string]any) string {
	if ip, ok := m["ip"].(map[string]any); ok {
		if addr, ok := ip["addr"].(string); ok && addr != "" {
			return addr
		}
	}
	if fqdn, ok := m["fqdn"].(string); ok {
		return fqdn
	}
	if hostname, ok := m["hostname"].(string); ok {
		return hostname
	}
	return ""
}

func regionMembers(ep models.RoutingEndpoint) (map[string]string, error) {
	raw, ok := ep.Config["regionMembers"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("avi_gslb: config field \"regionMembers\" is required " +
			"(map of region id to GSLB pool member IP or FQDN)")
	}
	out := make(map[string]string, len(raw))
	for region, v := range raw {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("avi_gslb: regionMembers[%s] must be a non-empty string", region)
		}
		out[region] = s
	}
	return out, nil
}

func redactURL(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[:i]
	}
	return u
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
