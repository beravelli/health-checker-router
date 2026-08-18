package arc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53recoverycluster"
	arctypes "github.com/aws/aws-sdk-go-v2/service/route53recoverycluster/types"

	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/routing"
)

// fakeEndpoint stands in for one of the five ARC cluster endpoints.
type fakeEndpoint struct {
	// err, when set, is what this endpoint returns — simulating a Regional
	// endpoint that is unavailable during the very event we are responding to.
	err error
	// states is the routing control state this endpoint reports.
	states map[string]arctypes.RoutingControlState

	updateCalls int
	getCalls    int
	lastEntries []arctypes.UpdateRoutingControlStateEntry
	lastInput   *route53recoverycluster.UpdateRoutingControlStatesInput
}

func (f *fakeEndpoint) UpdateRoutingControlStates(_ context.Context,
	in *route53recoverycluster.UpdateRoutingControlStatesInput,
	_ ...func(*route53recoverycluster.Options),
) (*route53recoverycluster.UpdateRoutingControlStatesOutput, error) {
	f.updateCalls++
	f.lastInput = in
	if f.err != nil {
		return nil, f.err
	}
	f.lastEntries = in.UpdateRoutingControlStateEntries
	if f.states == nil {
		f.states = map[string]arctypes.RoutingControlState{}
	}
	for _, e := range in.UpdateRoutingControlStateEntries {
		f.states[aws.ToString(e.RoutingControlArn)] = e.RoutingControlState
	}
	return &route53recoverycluster.UpdateRoutingControlStatesOutput{}, nil
}

func (f *fakeEndpoint) GetRoutingControlState(_ context.Context,
	in *route53recoverycluster.GetRoutingControlStateInput,
	_ ...func(*route53recoverycluster.Options),
) (*route53recoverycluster.GetRoutingControlStateOutput, error) {
	f.getCalls++
	if f.err != nil {
		return nil, f.err
	}
	return &route53recoverycluster.GetRoutingControlStateOutput{
		RoutingControlArn:   in.RoutingControlArn,
		RoutingControlState: f.states[aws.ToString(in.RoutingControlArn)],
	}, nil
}

var testControls = map[string]string{
	"us-east-1": "arn:aws:route53-recovery-control::111:controlpanel/x/routingcontrol/east",
	"us-west-2": "arn:aws:route53-recovery-control::111:controlpanel/x/routingcontrol/west",
}

func healthyCluster(n int) ([]DataPlaneAPI, []*fakeEndpoint) {
	clients := make([]DataPlaneAPI, n)
	fakes := make([]*fakeEndpoint, n)
	for i := range clients {
		f := &fakeEndpoint{states: map[string]arctypes.RoutingControlState{}}
		fakes[i] = f
		clients[i] = f
	}
	return clients, fakes
}

// TestFailsOverAcrossClusterEndpoints is the reason this adapter exists.
//
// ARC guarantees three of five endpoints reachable, and AWS instructs callers
// to retry each Regional endpoint in sequence. An adapter that gave up on the
// first error would be useless during exactly the regional event it is meant to
// respond to.
func TestFailsOverAcrossClusterEndpoints(t *testing.T) {
	clients, fakes := healthyCluster(5)
	// The first two Regional endpoints are down — the worst case ARC still
	// promises to survive.
	fakes[0].err = errors.New("endpoint unreachable")
	fakes[1].err = errors.New("connection timed out")

	a := New(clients, testControls)
	if err := a.Failover(context.Background(), "us-west-2"); err != nil {
		t.Fatalf("failover should have succeeded via a later endpoint: %v", err)
	}

	if fakes[0].updateCalls != 1 || fakes[1].updateCalls != 1 {
		t.Error("the failing endpoints should each have been tried once")
	}
	if fakes[2].updateCalls != 1 {
		t.Error("the third endpoint should have served the request")
	}
	// Once one succeeds, stop. Applying the same change five times is pointless
	// load on a cluster that may already be degraded.
	if fakes[3].updateCalls != 0 || fakes[4].updateCalls != 0 {
		t.Error("endpoints after the successful one should not have been called")
	}
}

func TestAllEndpointsFailingReportsEveryAttempt(t *testing.T) {
	clients, fakes := healthyCluster(5)
	for i, f := range fakes {
		f.err = errors.New("endpoint " + string(rune('a'+i)) + " down")
	}

	a := New(clients, testControls)
	err := a.Failover(context.Background(), "us-west-2")
	if err == nil {
		t.Fatal("expected an error when every endpoint is down")
	}
	// The operator needs to see that all five were tried, not just the last
	// failure — "one endpoint is flaky" and "the cluster is unreachable" call
	// for different responses.
	if !strings.Contains(err.Error(), "all 5 cluster endpoints failed") {
		t.Errorf("error should say every endpoint was tried, got: %v", err)
	}
	for i, f := range fakes {
		if f.updateCalls != 1 {
			t.Errorf("endpoint %d called %d times, want 1", i, f.updateCalls)
		}
	}
}

// TestSafetyRuleViolationIsNotRetried: a ConflictException is ARC deliberately
// refusing the change, usually because it would take every cell out of
// rotation. Retrying against the other four turns one correct refusal into five
// and cannot change the answer.
func TestSafetyRuleViolationIsNotRetried(t *testing.T) {
	clients, fakes := healthyCluster(5)
	fakes[0].err = &arctypes.ConflictException{Message: aws.String("safety rule violation")}

	a := New(clients, testControls)
	err := a.Failover(context.Background(), "us-west-2")
	if err == nil {
		t.Fatal("expected the safety-rule rejection to surface")
	}
	if !strings.Contains(err.Error(), "safety rule") {
		t.Errorf("error should name the safety rule, got: %v", err)
	}
	if fakes[1].updateCalls != 0 {
		t.Error("a safety-rule rejection must not be retried against other endpoints")
	}
}

// TestFailoverIsAtomic: the On and the Off travel in one call. Sequenced calls
// leave a window where both regions are Off, and ARC evaluates safety rules per
// call, so each step can be individually valid while the intermediate state is
// a full outage.
func TestFailoverIsAtomic(t *testing.T) {
	clients, fakes := healthyCluster(3)
	a := New(clients, testControls)

	if err := a.Failover(context.Background(), "us-west-2"); err != nil {
		t.Fatalf("failover: %v", err)
	}
	if fakes[0].updateCalls != 1 {
		t.Fatalf("made %d calls, want exactly 1 batched call", fakes[0].updateCalls)
	}
	if len(fakes[0].lastEntries) != 2 {
		t.Fatalf("batched %d entries, want both regions in one call", len(fakes[0].lastEntries))
	}

	got := map[string]arctypes.RoutingControlState{}
	for _, e := range fakes[0].lastEntries {
		got[aws.ToString(e.RoutingControlArn)] = e.RoutingControlState
	}
	if got[testControls["us-west-2"]] != arctypes.RoutingControlStateOn {
		t.Error("the failover target should be On")
	}
	if got[testControls["us-east-1"]] != arctypes.RoutingControlStateOff {
		t.Error("the evacuated region should be Off")
	}
}

// TestSetWeightCollapsesToBinary documents the impedance mismatch honestly:
// routing controls cannot express a proportion, so any non-zero weight is On.
func TestSetWeightCollapsesToBinary(t *testing.T) {
	cases := []struct {
		name  string
		w     routing.Weights
		wantE arctypes.RoutingControlState
		wantW arctypes.RoutingControlState
	}{
		{"even split", routing.Weights{"us-east-1": 50, "us-west-2": 50},
			arctypes.RoutingControlStateOn, arctypes.RoutingControlStateOn},
		{"lopsided split is still both On", routing.Weights{"us-east-1": 99, "us-west-2": 1},
			arctypes.RoutingControlStateOn, arctypes.RoutingControlStateOn},
		{"zero means Off", routing.Weights{"us-east-1": 0, "us-west-2": 100},
			arctypes.RoutingControlStateOff, arctypes.RoutingControlStateOn},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clients, fakes := healthyCluster(3)
			a := New(clients, testControls)
			if err := a.SetWeight(context.Background(), tc.w); err != nil {
				t.Fatalf("set weight: %v", err)
			}
			got := map[string]arctypes.RoutingControlState{}
			for _, e := range fakes[0].lastEntries {
				got[aws.ToString(e.RoutingControlArn)] = e.RoutingControlState
			}
			if got[testControls["us-east-1"]] != tc.wantE {
				t.Errorf("us-east-1 = %s, want %s", got[testControls["us-east-1"]], tc.wantE)
			}
			if got[testControls["us-west-2"]] != tc.wantW {
				t.Errorf("us-west-2 = %s, want %s", got[testControls["us-west-2"]], tc.wantW)
			}
		})
	}
}

func TestSetWeightRejectsInvalidWeights(t *testing.T) {
	clients, fakes := healthyCluster(3)
	a := New(clients, testControls)

	// Does not sum to 100. Rejected before any call reaches AWS.
	if err := a.SetWeight(context.Background(), routing.Weights{"us-east-1": 60, "us-west-2": 60}); err == nil {
		t.Error("expected validation to reject weights summing to 120")
	}
	if fakes[0].updateCalls != 0 {
		t.Error("invalid weights should never reach the data plane")
	}
}

func TestUnknownRegionIsRejected(t *testing.T) {
	clients, fakes := healthyCluster(3)
	a := New(clients, testControls)

	if err := a.Failover(context.Background(), "eu-west-1"); err == nil {
		t.Error("expected an error for a region with no routing control ARN")
	}
	if fakes[0].updateCalls != 0 {
		t.Error("an unmapped region should not produce a data-plane call")
	}

	err := a.SetWeight(context.Background(), routing.Weights{"eu-west-1": 100})
	if err == nil || !strings.Contains(err.Error(), "eu-west-1") {
		t.Errorf("SetWeight should name the unmapped region, got: %v", err)
	}
}

// ---------------------------------------------------------------- status

func TestStatusReportsRotation(t *testing.T) {
	clients, fakes := healthyCluster(3)
	for _, f := range fakes {
		f.states = map[string]arctypes.RoutingControlState{
			testControls["us-east-1"]: arctypes.RoutingControlStateOn,
			testControls["us-west-2"]: arctypes.RoutingControlStateOn,
		}
	}

	a := New(clients, testControls)
	st, err := a.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Reachable {
		t.Fatal("cluster should be reachable")
	}
	if st.Provider != models.ProviderAWSARC {
		t.Errorf("provider = %s", st.Provider)
	}
	// Both On, so the honest translation is an even split summing to 100.
	if st.Weights["us-east-1"]+st.Weights["us-west-2"] != 100 {
		t.Errorf("weights = %v, want a 100 total", st.Weights)
	}
}

func TestStatusEvacuatedRegionReportsZero(t *testing.T) {
	clients, fakes := healthyCluster(3)
	for _, f := range fakes {
		f.states = map[string]arctypes.RoutingControlState{
			testControls["us-east-1"]: arctypes.RoutingControlStateOff,
			testControls["us-west-2"]: arctypes.RoutingControlStateOn,
		}
	}

	st, err := New(clients, testControls).Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Weights["us-east-1"] != 0 {
		t.Errorf("evacuated region weight = %d, want 0", st.Weights["us-east-1"])
	}
	if st.Weights["us-west-2"] != 100 {
		t.Errorf("in-rotation region weight = %d, want 100", st.Weights["us-west-2"])
	}
}

// TestStatusAllOffIsReportedNotHidden: every control Off means no region is
// serving. That must be visible rather than smoothed into an empty weight map.
func TestStatusAllOffIsReportedNotHidden(t *testing.T) {
	clients, fakes := healthyCluster(3)
	for _, f := range fakes {
		f.states = map[string]arctypes.RoutingControlState{
			testControls["us-east-1"]: arctypes.RoutingControlStateOff,
			testControls["us-west-2"]: arctypes.RoutingControlStateOff,
		}
	}

	st, err := New(clients, testControls).Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(st.Detail, "no region is in rotation") {
		t.Errorf("detail = %q; an all-Off cluster must say so plainly", st.Detail)
	}
}

func TestStatusUnreachableClusterIsNotAnError(t *testing.T) {
	clients, fakes := healthyCluster(3)
	for _, f := range fakes {
		f.err = errors.New("unreachable")
	}

	// Status reports unreachability in the struct rather than as an error, so
	// the console can render "cannot reach ARC" instead of failing the page.
	st, err := New(clients, testControls).Status(context.Background())
	if err != nil {
		t.Fatalf("status should not error on an unreachable cluster: %v", err)
	}
	if st.Reachable {
		t.Error("cluster should be reported unreachable")
	}
	if st.Detail == "" {
		t.Error("unreachable status should carry a reason")
	}
}

// ---------------------------------------------------------------- config

func TestFactoryRejectsTooFewEndpoints(t *testing.T) {
	// Configuring fewer than three forfeits the quorum guarantee that is the
	// entire reason to use ARC over a record write.
	_, err := clusterEndpoints(models.RoutingEndpoint{
		Provider: models.ProviderAWSARC,
		Config: map[string]any{
			"clusterEndpoints": []any{
				map[string]any{"endpoint": "https://a.example", "region": "us-east-1"},
				map[string]any{"endpoint": "https://b.example", "region": "us-west-2"},
			},
		},
	})
	if err == nil {
		t.Fatal("expected two endpoints to be rejected")
	}
	if !strings.Contains(err.Error(), "forfeits") {
		t.Errorf("error should explain why, got: %v", err)
	}
}

func TestClusterEndpointsParsesFive(t *testing.T) {
	raw := []any{}
	for _, r := range []string{"us-east-1", "us-west-2", "eu-west-1", "ap-northeast-1", "ap-southeast-2"} {
		raw = append(raw, map[string]any{"endpoint": "https://" + r + ".example", "region": r})
	}
	eps, err := clusterEndpoints(models.RoutingEndpoint{Config: map[string]any{"clusterEndpoints": raw}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(eps) != 5 {
		t.Fatalf("got %d endpoints, want 5", len(eps))
	}
	// Order is preserved: it is the retry order, so it is meaningful.
	if eps[0].Region != "us-east-1" || eps[4].Region != "ap-southeast-2" {
		t.Error("endpoint order should be preserved as configured")
	}
}

func TestRoutingControlsRequired(t *testing.T) {
	if _, err := routingControls(models.RoutingEndpoint{Config: map[string]any{}}); err == nil {
		t.Error("missing routingControls should be rejected")
	}
	if _, err := routingControls(models.RoutingEndpoint{
		Config: map[string]any{"routingControls": map[string]any{"us-east-1": ""}},
	}); err == nil {
		t.Error("an empty ARN should be rejected")
	}
}

// TestSafetyRulesNotOverriddenByDefault: a bug in weight computation that asked
// for every region Off should be refused by AWS, not obeyed. Overriding is
// opt-in.
func TestSafetyRulesNotOverriddenByDefault(t *testing.T) {
	clients, fakes := healthyCluster(3)
	a := New(clients, testControls)

	if err := a.Failover(context.Background(), "us-west-2"); err != nil {
		t.Fatalf("failover: %v", err)
	}
	if len(fakes[0].lastInput.SafetyRulesToOverride) != 0 {
		t.Errorf("safety rules overridden by default: %v", fakes[0].lastInput.SafetyRulesToOverride)
	}
}

func TestNoEndpointsConfigured(t *testing.T) {
	a := New(nil, testControls)
	if err := a.Failover(context.Background(), "us-west-2"); err == nil {
		t.Error("expected an error with no cluster endpoints")
	}
}
