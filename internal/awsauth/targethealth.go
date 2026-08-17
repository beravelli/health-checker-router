package awsauth

import (
	"context"
	"fmt"

	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"github.com/beravelli/health-checker-router/internal/models"
)

// TargetHealth queries ELBv2 target-group health.
//
// It implements prober.TargetHealthChecker, which is why that interface lives
// in the prober package: the prober does not import the AWS SDK.
type TargetHealth struct {
	resolver *Resolver
}

// NewTargetHealth returns a checker backed by resolver.
func NewTargetHealth(r *Resolver) *TargetHealth { return &TargetHealth{resolver: r} }

// TargetGroupHealth reports the aggregate health of a service's target group
// in one region.
//
// The mapping is deliberately graded rather than binary: some unhealthy
// targets behind a load balancer is a degraded service, not a dead one, and
// collapsing that to "critical" would trigger a failover the situation does
// not warrant.
func (t *TargetHealth) TargetGroupHealth(ctx context.Context, svc models.Service, region string) (models.Status, string, error) {
	arn, _ := svc.ProbeConfig["targetGroupArn"].(string)
	if arn == "" {
		return models.StatusUnknown, "", fmt.Errorf("service %s: probeConfig.targetGroupArn is required", svc.Name)
	}

	cfg, err := t.resolver.Config(ctx, SpecFromService(svc, region))
	if err != nil {
		return models.StatusUnknown, "", fmt.Errorf("resolve AWS credentials: %w", err)
	}

	client := elbv2.NewFromConfig(cfg)
	out, err := client.DescribeTargetHealth(ctx, &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: &arn,
	})
	if err != nil {
		// An API failure tells us about our access to AWS, not about the
		// targets. Unknown, so fail-open governs the response.
		return models.StatusUnknown, "", fmt.Errorf("describe target health: %w", err)
	}

	total := len(out.TargetHealthDescriptions)
	if total == 0 {
		return models.StatusCritical, "target group has no registered targets", nil
	}

	var healthy, draining, unhealthy int
	for _, d := range out.TargetHealthDescriptions {
		if d.TargetHealth == nil {
			continue
		}
		switch d.TargetHealth.State {
		case elbtypes.TargetHealthStateEnumHealthy:
			healthy++
		case elbtypes.TargetHealthStateEnumDraining, elbtypes.TargetHealthStateEnumInitial:
			// In-flight lifecycle states are not failures.
			draining++
		default:
			unhealthy++
		}
	}

	detail := fmt.Sprintf("%d/%d healthy", healthy, total)
	if draining > 0 {
		detail += fmt.Sprintf(", %d draining/initial", draining)
	}

	// Everything in a transitional state and nothing confirmed either way.
	if healthy == 0 && unhealthy == 0 {
		return models.StatusUnknown, detail, nil
	}
	if healthy == 0 {
		return models.StatusCritical, detail, nil
	}

	// Below half the fleet healthy is a real capacity problem; anything short
	// of full health is degraded.
	ratio := float64(healthy) / float64(healthy+unhealthy)
	switch {
	case ratio >= 1.0:
		return models.StatusHealthy, detail, nil
	case ratio < 0.5:
		return models.StatusCritical, detail, nil
	default:
		return models.StatusDegraded, detail, nil
	}
}
