// Package akamai routes traffic with an Akamai Application Load Balancer
// Cloudlet policy.
//
// Changing traffic on Akamai means creating a new policy version with updated
// origin percentages and activating it — versions are immutable, so this is a
// create-then-activate flow rather than an in-place edit.
//
// Requests are signed with EdgeGrid. The signing implementation is inlined
// here (it is ~40 lines of HMAC) rather than pulling in the Akamai SDK, which
// would otherwise be the only dependency in the tree that wants to read
// ~/.edgerc off disk — this service resolves credentials from a secret
// reference instead.
package akamai

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/beravelli/health-checker-router/internal/config"
	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/routing"

	"github.com/google/uuid"
)

// credentials is the EdgeGrid credential bundle, parsed from the JSON blob the
// secret reference points at.
type credentials struct {
	ClientToken  string `json:"client_token"`
	ClientSecret string `json:"client_secret"`
	AccessToken  string `json:"access_token"`
	Host         string `json:"host"`
}

func (c credentials) validate() error {
	var missing []string
	if c.ClientToken == "" {
		missing = append(missing, "client_token")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "client_secret")
	}
	if c.AccessToken == "" {
		missing = append(missing, "access_token")
	}
	if c.Host == "" {
		missing = append(missing, "host")
	}
	if len(missing) > 0 {
		return fmt.Errorf("EdgeGrid credential bundle is missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Adapter implements routing.Provider against Akamai Cloudlets.
type Adapter struct {
	client   *http.Client
	creds    credentials
	policyID string
	network  string // "staging" or "production"
	// regionOrigins maps region id to the Cloudlet origin id.
	regionOrigins map[string]string
}

// Factory builds an Adapter from stored configuration.
func Factory() routing.Factory {
	return func(ctx context.Context, ep models.RoutingEndpoint) (routing.Provider, error) {
		policyID, err := routing.ConfigString(ep, "policyId", true)
		if err != nil {
			return nil, err
		}
		network, _ := routing.ConfigString(ep, "network", false)
		if network == "" {
			network = "production"
		}
		if network != "staging" && network != "production" {
			return nil, fmt.Errorf("akamai_cloudlet: network must be \"staging\" or \"production\", got %q", network)
		}

		origins, err := regionOrigins(ep)
		if err != nil {
			return nil, err
		}

		raw, err := config.Resolve(ep.AuthRef)
		if err != nil {
			return nil, fmt.Errorf("akamai_cloudlet: resolve EdgeGrid bundle: %w", err)
		}
		var creds credentials
		if err := json.Unmarshal([]byte(raw), &creds); err != nil {
			return nil, fmt.Errorf("akamai_cloudlet: EdgeGrid bundle in %s is not valid JSON: %w", ep.AuthRef, err)
		}
		if err := creds.validate(); err != nil {
			return nil, fmt.Errorf("akamai_cloudlet: %w", err)
		}

		return &Adapter{
			client:        &http.Client{Timeout: 30 * time.Second},
			creds:         creds,
			policyID:      policyID,
			network:       network,
			regionOrigins: origins,
		}, nil
	}
}

// Name identifies the provider.
func (a *Adapter) Name() models.RoutingProvider { return models.ProviderAkamaiCloudlet }

// matchRule is one Cloudlet ALB rule with per-origin percentages.
type matchRule struct {
	Type            string           `json:"type"`
	Name            string           `json:"name"`
	ForwardSettings forwardSettings  `json:"forwardSettings"`
	MatchURL        string           `json:"matchURL,omitempty"`
	Matches         []map[string]any `json:"matches,omitempty"`
}

type forwardSettings struct {
	OriginID string `json:"originId"`
}

// dataCenter is one weighted origin in the load-balancing configuration.
type dataCenter struct {
	OriginID     string  `json:"originId"`
	Percent      float64 `json:"percent"`
	Hostname     string  `json:"hostname,omitempty"`
	Country      string  `json:"country,omitempty"`
	Continent    string  `json:"continent,omitempty"`
	CloudService bool    `json:"cloudService,omitempty"`
}

// SetWeight creates and activates a new policy version with the given split.
func (a *Adapter) SetWeight(ctx context.Context, w routing.Weights) error {
	if err := w.Validate(); err != nil {
		return fmt.Errorf("akamai_cloudlet: %w", err)
	}

	centers := make([]dataCenter, 0, len(w))
	for _, region := range w.Regions() {
		originID, ok := a.regionOrigins[region]
		if !ok {
			return fmt.Errorf("akamai_cloudlet: no origin configured for region %q", region)
		}
		centers = append(centers, dataCenter{
			OriginID: originID,
			Percent:  float64(w[region]),
		})
	}

	version, err := a.createVersion(ctx, centers,
		fmt.Sprintf("health-checker-router weight update %s", time.Now().UTC().Format(time.RFC3339)))
	if err != nil {
		return err
	}
	return a.activate(ctx, version)
}

// Failover sends all traffic to one region.
func (a *Adapter) Failover(ctx context.Context, toRegion string) error {
	if _, ok := a.regionOrigins[toRegion]; !ok {
		return fmt.Errorf("akamai_cloudlet: unknown failover target region %q", toRegion)
	}
	w := routing.Weights{}
	for region := range a.regionOrigins {
		w[region] = 0
	}
	w[toRegion] = 100
	return a.SetWeight(ctx, w)
}

// Status reads back the live policy version's split.
func (a *Adapter) Status(ctx context.Context) (routing.ProviderStatus, error) {
	st := routing.ProviderStatus{Provider: models.ProviderAkamaiCloudlet, Weights: routing.Weights{}}

	path := fmt.Sprintf("/cloudlets/v3/policies/%s/versions?page=0&size=1", a.policyID)
	body, err := a.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		st.Detail = err.Error()
		return st, err
	}
	st.Reachable = true

	var listing struct {
		Content []struct {
			Version                int `json:"version"`
			MatchRulesLoadBalancer struct {
				DataCenters []dataCenter `json:"dataCenters"`
			} `json:"loadBalancer"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return st, fmt.Errorf("akamai_cloudlet: decode versions: %w", err)
	}

	originToRegion := map[string]string{}
	for region, origin := range a.regionOrigins {
		originToRegion[origin] = region
	}
	if len(listing.Content) > 0 {
		for _, dc := range listing.Content[0].MatchRulesLoadBalancer.DataCenters {
			if region, ok := originToRegion[dc.OriginID]; ok {
				st.Weights[region] = int(dc.Percent)
			}
		}
		st.Detail = fmt.Sprintf("policy %s version %d on %s",
			a.policyID, listing.Content[0].Version, a.network)
	}
	return st, nil
}

// createVersion creates an immutable policy version and returns its number.
func (a *Adapter) createVersion(ctx context.Context, centers []dataCenter, description string) (int, error) {
	payload := map[string]any{
		"description": description,
		"loadBalancer": map[string]any{
			"dataCenters": centers,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("akamai_cloudlet: encode version payload: %w", err)
	}

	path := fmt.Sprintf("/cloudlets/v3/policies/%s/versions", a.policyID)
	respBody, err := a.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return 0, err
	}

	var created struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return 0, fmt.Errorf("akamai_cloudlet: decode created version: %w", err)
	}
	if created.Version == 0 {
		return 0, fmt.Errorf("akamai_cloudlet: policy version was created without a version number")
	}
	return created.Version, nil
}

// activate promotes a policy version onto the configured network.
func (a *Adapter) activate(ctx context.Context, version int) error {
	payload, err := json.Marshal(map[string]any{
		"network":       strings.ToUpper(a.network),
		"operation":     "ACTIVATION",
		"policyVersion": version,
	})
	if err != nil {
		return fmt.Errorf("akamai_cloudlet: encode activation payload: %w", err)
	}

	path := fmt.Sprintf("/cloudlets/v3/policies/%s/activations", a.policyID)
	if _, err := a.do(ctx, http.MethodPost, path, payload); err != nil {
		return fmt.Errorf("akamai_cloudlet: activate version %d: %w", version, err)
	}
	return nil
}

// do issues an EdgeGrid-signed request.
func (a *Adapter) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	full := "https://" + a.creds.Host + path

	req, err := http.NewRequestWithContext(ctx, method, full, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("akamai_cloudlet: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if err := a.sign(req, body); err != nil {
		return nil, err
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("akamai_cloudlet: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := new(bytes.Buffer)
	if _, err := out.ReadFrom(resp.Body); err != nil {
		return nil, fmt.Errorf("akamai_cloudlet: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("akamai_cloudlet: %s %s returned %d: %s",
			method, path, resp.StatusCode, truncate(out.String(), 400))
	}
	return out.Bytes(), nil
}

// sign applies the EdgeGrid Authorization header.
//
// EdgeGrid signs a canonical request string with a per-request key derived
// from the client secret and timestamp. See Akamai's "Authenticate with
// EdgeGrid" specification.
func (a *Adapter) sign(req *http.Request, body []byte) error {
	timestamp := time.Now().UTC().Format("20060102T15:04:05+0000")
	nonce := uuid.NewString()

	authHeader := fmt.Sprintf(
		"EG1-HMAC-SHA256 client_token=%s;access_token=%s;timestamp=%s;nonce=%s;",
		a.creds.ClientToken, a.creds.AccessToken, timestamp, nonce)

	u, err := url.Parse(req.URL.String())
	if err != nil {
		return fmt.Errorf("akamai_cloudlet: parse request URL: %w", err)
	}
	pathWithQuery := u.EscapedPath()
	if u.RawQuery != "" {
		pathWithQuery += "?" + u.RawQuery
	}

	// Only POST/PUT bodies are hashed, and only up to Akamai's 128KB limit.
	contentHash := ""
	if req.Method == http.MethodPost || req.Method == http.MethodPut {
		const maxBody = 128 * 1024
		payload := body
		if len(payload) > maxBody {
			payload = payload[:maxBody]
		}
		sum := sha256.Sum256(payload)
		contentHash = base64.StdEncoding.EncodeToString(sum[:])
	}

	dataToSign := strings.Join([]string{
		req.Method,
		"https",
		a.creds.Host,
		pathWithQuery,
		"", // canonicalized headers: none signed
		contentHash,
		authHeader,
	}, "\t")

	signingKey := hmacSHA256(a.creds.ClientSecret, timestamp)
	signature := hmacSHA256(signingKey, dataToSign)

	req.Header.Set("Authorization", authHeader+"signature="+signature)
	return nil
}

func hmacSHA256(key, data string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(data))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func regionOrigins(ep models.RoutingEndpoint) (map[string]string, error) {
	raw, ok := ep.Config["regionOrigins"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("akamai_cloudlet: config field \"regionOrigins\" is required " +
			"(map of region id to Cloudlet origin id)")
	}
	out := make(map[string]string, len(raw))
	for region, v := range raw {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("akamai_cloudlet: regionOrigins[%s] must be a non-empty string", region)
		}
		out[region] = s
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
