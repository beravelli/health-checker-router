// Package arc routes traffic with Amazon Application Recovery Controller
// routing controls.
//
// This adapter exists because the other AWS adapter cannot be trusted during
// the event it is for. Route 53's record-mutation APIs — ChangeResourceRecordSets
// included — are control-plane operations hosted in us-east-1. AWS names
// "update the weight of a weighted record to perform failover" as the canonical
// non-statically-stable pattern precisely because of this: if us-east-1 is
// degraded, the call that would evacuate us-east-1 is the call that fails.
//
// ARC routing controls invert that. The routing control data plane is a cluster
// of five endpoints in five geographically isolated Regions, operating on a
// quorum model that keeps functioning with up to two endpoints unavailable. You
// flip a control rather than write a record, and Route 53 health checks
// associated with each control change what DNS answers — all in the data plane.
//
// # Binary, not proportional
//
// Routing controls are On/Off. They cannot express "70/30", and pretending
// otherwise would be the worst kind of abstraction leak in a system whose whole
// premise is proportional weighting.
//
// So this adapter is deliberately not a drop-in replacement for the weighted
// providers. It answers one question — is this region in rotation — which is
// exactly the question that has to be answerable when a region is on fire.
// SetWeight maps any non-zero weight to On and zero to Off; graduated shifting
// stays with a weighted provider. The two are complementary:
//
//   - Degradation, proportional response: Route 53 weights or the Envoy edge.
//   - Evacuation, must work during a regional event: ARC.
//
// # Safety rules
//
// ARC assertion rules can prevent a state change that would take every cell out
// of rotation. That protection is server-side and this adapter never overrides
// it by default: a bug here that computed all-zero weights should be refused by
// AWS, not obeyed. Overrides are opt-in per endpoint config and logged.
package arc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53recoverycluster"
	arctypes "github.com/aws/aws-sdk-go-v2/service/route53recoverycluster/types"

	"github.com/beravelli/health-checker-router/internal/awsauth"
	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/routing"
)

// DataPlaneAPI is the subset of the ARC cluster data plane this adapter uses.
// An interface so the sequential-failover logic can be tested without AWS.
type DataPlaneAPI interface {
	UpdateRoutingControlStates(ctx context.Context,
		in *route53recoverycluster.UpdateRoutingControlStatesInput,
		optFns ...func(*route53recoverycluster.Options)) (*route53recoverycluster.UpdateRoutingControlStatesOutput, error)
	GetRoutingControlState(ctx context.Context,
		in *route53recoverycluster.GetRoutingControlStateInput,
		optFns ...func(*route53recoverycluster.Options)) (*route53recoverycluster.GetRoutingControlStateOutput, error)
}

// endpoint pairs a cluster endpoint URL with the AWS region that serves it.
// Both are needed: the SDK signs against the region, and the URL selects which
// of the five cluster endpoints receives the call.
type endpoint struct {
	URL    string
	Region string
}

// Adapter implements routing.Provider against ARC routing controls.
type Adapter struct {
	// clients is one client per cluster endpoint, in the order they should be
	// tried. AWS guarantees at least three of five are reachable, and
	// explicitly instructs callers to retry each Regional endpoint in sequence
	// rather than relying on a single one.
	clients []DataPlaneAPI
	// endpoints parallels clients, for error messages.
	endpoints []endpoint

	// controlARNs maps region id to its routing control ARN.
	controlARNs map[string]string

	// safetyRulesToOverride is normally empty. Populated only when an operator
	// has deliberately accepted that a state change may take every cell out of
	// rotation.
	safetyRulesToOverride []string
}

// Factory builds an Adapter from stored configuration.
//
// Required config:
//
//	clusterEndpoints  [{"endpoint": "https://...", "region": "us-east-1"}, ...]
//	routingControls   {"us-east-1": "arn:aws:route53-recovery-control::...", ...}
//
// Optional:
//
//	safetyRulesToOverride  ["arn:..."]  — opt-in, logged, normally absent
func Factory(resolver *awsauth.Resolver) routing.Factory {
	return func(ctx context.Context, ep models.RoutingEndpoint) (routing.Provider, error) {
		endpoints, err := clusterEndpoints(ep)
		if err != nil {
			return nil, err
		}
		controls, err := routingControls(ep)
		if err != nil {
			return nil, err
		}

		clients := make([]DataPlaneAPI, 0, len(endpoints))
		for _, e := range endpoints {
			cfg, err := resolver.Config(ctx, awsauth.Spec{
				Method:     models.AWSAuthMethod(ep.AuthMethod),
				Ref:        ep.AuthRef,
				ExternalID: stringField(ep.Config, "externalId"),
				Region:     e.Region,
			})
			if err != nil {
				return nil, fmt.Errorf("arc: resolve credentials for %s: %w", e.Region, err)
			}
			url := e.URL
			clients = append(clients, route53recoverycluster.NewFromConfig(cfg,
				func(o *route53recoverycluster.Options) {
					o.BaseEndpoint = aws.String(url)
				}))
		}

		return &Adapter{
			clients:               clients,
			endpoints:             endpoints,
			controlARNs:           controls,
			safetyRulesToOverride: stringsField(ep.Config, "safetyRulesToOverride"),
		}, nil
	}
}

// New builds an Adapter directly from clients, for tests.
func New(clients []DataPlaneAPI, controlARNs map[string]string) *Adapter {
	eps := make([]endpoint, len(clients))
	for i := range clients {
		eps[i] = endpoint{URL: fmt.Sprintf("client-%d", i), Region: "test"}
	}
	return &Adapter{clients: clients, endpoints: eps, controlARNs: controlARNs}
}

// Name identifies the provider.
func (a *Adapter) Name() models.RoutingProvider { return models.ProviderAWSARC }

// SetWeight puts each region in or out of rotation.
//
// Routing controls are binary, so a weight is collapsed: non-zero means On,
// zero means Off. A 70/30 split and a 50/50 split produce the same routing
// control states — both regions On — which is correct for what this mechanism
// controls. Pair it with a weighted provider if you need the proportion applied
// as well.
func (a *Adapter) SetWeight(ctx context.Context, w routing.Weights) error {
	if err := w.Validate(); err != nil {
		return fmt.Errorf("arc: %w", err)
	}

	entries := make([]arctypes.UpdateRoutingControlStateEntry, 0, len(w))
	for _, region := range w.Regions() {
		arn, ok := a.controlARNs[region]
		if !ok {
			return fmt.Errorf("arc: no routing control ARN configured for region %q", region)
		}
		entries = append(entries, arctypes.UpdateRoutingControlStateEntry{
			RoutingControlArn:   aws.String(arn),
			RoutingControlState: stateFor(w[region]),
		})
	}
	return a.updateStates(ctx, entries)
}

// Failover puts toRegion in rotation and takes every other configured region
// out, in a single batched call.
//
// Batched deliberately: sending the On and the Off separately leaves a window
// where both are Off, and ARC's safety rules evaluate each call independently,
// so a sequenced pair can be individually valid while the intermediate state is
// an outage.
func (a *Adapter) Failover(ctx context.Context, toRegion string) error {
	if _, ok := a.controlARNs[toRegion]; !ok {
		return fmt.Errorf("arc: no routing control ARN configured for region %q", toRegion)
	}

	entries := make([]arctypes.UpdateRoutingControlStateEntry, 0, len(a.controlARNs))
	for _, region := range sortedKeys(a.controlARNs) {
		state := arctypes.RoutingControlStateOff
		if region == toRegion {
			state = arctypes.RoutingControlStateOn
		}
		entries = append(entries, arctypes.UpdateRoutingControlStateEntry{
			RoutingControlArn:   aws.String(a.controlARNs[region]),
			RoutingControlState: state,
		})
	}
	return a.updateStates(ctx, entries)
}

// Status reads the live routing control states.
//
// Weights are reported as 100 divided evenly across the regions that are On,
// which is the honest translation: ARC knows in-rotation and out-of-rotation,
// not proportions. A caller comparing this against a weighted provider's view
// should expect them to differ, and that difference is information rather than
// drift.
func (a *Adapter) Status(ctx context.Context) (routing.ProviderStatus, error) {
	st := routing.ProviderStatus{Provider: models.ProviderAWSARC, Weights: routing.Weights{}}

	on := make([]string, 0, len(a.controlARNs))
	for _, region := range sortedKeys(a.controlARNs) {
		state, err := a.getState(ctx, a.controlARNs[region])
		if err != nil {
			st.Reachable = false
			st.Detail = err.Error()
			return st, nil
		}
		st.Weights[region] = 0
		if state == arctypes.RoutingControlStateOn {
			on = append(on, region)
		}
	}

	st.Reachable = true
	switch len(on) {
	case 0:
		st.Detail = "every routing control is Off — no region is in rotation"
	default:
		share := 100 / len(on)
		remainder := 100 - share*len(on)
		for i, region := range on {
			st.Weights[region] = share
			if i == 0 {
				st.Weights[region] += remainder
			}
		}
		st.Detail = fmt.Sprintf("in rotation: %s", strings.Join(on, ", "))
	}
	return st, nil
}

// updateStates applies entries, trying each cluster endpoint in turn.
//
// This sequential failover is the entire point of the adapter. A single
// endpoint can be unavailable during exactly the regional event that requires
// the state change, and AWS's guidance is explicit that callers must retry each
// Regional endpoint rather than treating one as authoritative.
func (a *Adapter) updateStates(ctx context.Context, entries []arctypes.UpdateRoutingControlStateEntry) error {
	if len(a.clients) == 0 {
		return errors.New("arc: no cluster endpoints configured")
	}

	in := &route53recoverycluster.UpdateRoutingControlStatesInput{
		UpdateRoutingControlStateEntries: entries,
		SafetyRulesToOverride:            a.safetyRulesToOverride,
	}

	var attempts []string
	for i, client := range a.clients {
		if ctx.Err() != nil {
			return fmt.Errorf("arc: %w", ctx.Err())
		}
		_, err := client.UpdateRoutingControlStates(ctx, in)
		if err == nil {
			return nil
		}
		// A safety-rule rejection is the cluster working as intended, not an
		// unhealthy endpoint. Retrying it against the other four would turn one
		// deliberate refusal into five, and the answer will not change.
		if isSafetyRuleViolation(err) {
			return fmt.Errorf("arc: refused by a safety rule (this is ARC protecting you from an "+
				"all-cells-off state; override explicitly if intended): %w", err)
		}
		attempts = append(attempts, fmt.Sprintf("%s: %v", a.endpoints[i].Region, err))
	}
	return fmt.Errorf("arc: all %d cluster endpoints failed [%s]",
		len(a.clients), strings.Join(attempts, "; "))
}

// getState reads one routing control, trying each endpoint in turn.
func (a *Adapter) getState(ctx context.Context, arn string) (arctypes.RoutingControlState, error) {
	if len(a.clients) == 0 {
		return "", errors.New("arc: no cluster endpoints configured")
	}

	var attempts []string
	for i, client := range a.clients {
		if ctx.Err() != nil {
			return "", fmt.Errorf("arc: %w", ctx.Err())
		}
		out, err := client.GetRoutingControlState(ctx,
			&route53recoverycluster.GetRoutingControlStateInput{RoutingControlArn: aws.String(arn)})
		if err == nil {
			return out.RoutingControlState, nil
		}
		attempts = append(attempts, fmt.Sprintf("%s: %v", a.endpoints[i].Region, err))
	}
	return "", fmt.Errorf("arc: all %d cluster endpoints failed reading %s [%s]",
		len(a.clients), arn, strings.Join(attempts, "; "))
}

// stateFor collapses a weight to a routing control state.
func stateFor(weight int) arctypes.RoutingControlState {
	if weight > 0 {
		return arctypes.RoutingControlStateOn
	}
	return arctypes.RoutingControlStateOff
}

// isSafetyRuleViolation reports whether an error is ARC declining the change
// rather than failing to process it.
func isSafetyRuleViolation(err error) bool {
	var conflict *arctypes.ConflictException
	return errors.As(err, &conflict)
}

// ---------------------------------------------------------------- config

func clusterEndpoints(ep models.RoutingEndpoint) ([]endpoint, error) {
	raw, ok := ep.Config["clusterEndpoints"].([]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("arc: config field %q is required and must be a non-empty list "+
			"of {endpoint, region} objects", "clusterEndpoints")
	}

	out := make([]endpoint, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("arc: clusterEndpoints[%d] is not an object", i)
		}
		url, _ := m["endpoint"].(string)
		region, _ := m["region"].(string)
		if url == "" || region == "" {
			return nil, fmt.Errorf("arc: clusterEndpoints[%d] needs both %q and %q", i, "endpoint", "region")
		}
		out = append(out, endpoint{URL: url, Region: region})
	}

	// Fewer than three and the quorum guarantee is gone: ARC promises at least
	// three of five reachable, which is only useful if all five are configured.
	if len(out) < 3 {
		return nil, fmt.Errorf("arc: %d cluster endpoints configured; ARC clusters have five and "+
			"guarantee three reachable, so configuring fewer forfeits the availability this "+
			"adapter exists to provide", len(out))
	}
	return out, nil
}

func routingControls(ep models.RoutingEndpoint) (map[string]string, error) {
	raw, ok := ep.Config["routingControls"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("arc: config field %q is required and must map region id to "+
			"routing control ARN", "routingControls")
	}
	out := make(map[string]string, len(raw))
	for region, v := range raw {
		arn, _ := v.(string)
		if arn == "" {
			return nil, fmt.Errorf("arc: routingControls[%q] is empty", region)
		}
		out[region] = arn
	}
	return out, nil
}

func stringField(cfg map[string]any, key string) string {
	v, _ := cfg[key].(string)
	return v
}

func stringsField(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, _ := v.(string); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
