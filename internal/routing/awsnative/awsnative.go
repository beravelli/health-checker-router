// Package awsnative routes traffic with Route 53 weighted record sets.
//
// Route 53 is the weight mechanism of record, but it is explicitly *not* the
// failover mechanism: a record change is subject to client-side DNS caching,
// so a failover done purely in DNS leaves stale clients pointed at the pulled
// region for up to the TTL. The record TTL is therefore pinned low as a
// backstop, and the immediate cut is expected to happen at the Envoy edge via
// the LB controller. This adapter keeps DNS consistent with that decision.
package awsnative

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"

	"github.com/beravelli/health-checker-router/internal/awsauth"
	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/routing"
)

// recordTTL is intentionally short. DNS is the backstop, not the primary
// failover path, so a stale cache window is capped at 30s.
const recordTTL int64 = 30

// Adapter implements routing.Provider against Route 53.
type Adapter struct {
	client     *route53.Client
	hostedZone string
	recordName string
	recordType r53types.RRType
	// regionTargets maps region id to the DNS value served for that region
	// (an ALB/NLB hostname, typically).
	regionTargets map[string]string
}

// Factory builds an Adapter from stored configuration.
func Factory(resolver *awsauth.Resolver) routing.Factory {
	return func(ctx context.Context, ep models.RoutingEndpoint) (routing.Provider, error) {
		zone, err := routing.ConfigString(ep, "hostedZoneId", true)
		if err != nil {
			return nil, err
		}
		record, err := routing.ConfigString(ep, "recordName", true)
		if err != nil {
			return nil, err
		}
		region, _ := routing.ConfigString(ep, "apiRegion", false)
		if region == "" {
			region = "us-east-1" // Route 53 is a global service with a us-east-1 endpoint
		}

		targets, err := regionTargets(ep)
		if err != nil {
			return nil, err
		}

		cfg, err := resolver.Config(ctx, awsauth.Spec{
			Method:     models.AWSAuthMethod(ep.AuthMethod),
			Ref:        ep.AuthRef,
			ExternalID: stringField(ep.Config, "externalId"),
			Region:     region,
		})
		if err != nil {
			return nil, fmt.Errorf("aws_native: resolve credentials: %w", err)
		}

		return &Adapter{
			client:        route53.NewFromConfig(cfg),
			hostedZone:    zone,
			recordName:    record,
			recordType:    r53types.RRTypeCname,
			regionTargets: targets,
		}, nil
	}
}

// Name identifies the provider.
func (a *Adapter) Name() models.RoutingProvider { return models.ProviderAWSNative }

// SetWeight upserts one weighted record set per region in a single change
// batch, so the split is applied atomically rather than region by region.
func (a *Adapter) SetWeight(ctx context.Context, w routing.Weights) error {
	if err := w.Validate(); err != nil {
		return fmt.Errorf("aws_native: %w", err)
	}

	changes := make([]r53types.Change, 0, len(w))
	for _, region := range w.Regions() {
		target, ok := a.regionTargets[region]
		if !ok {
			return fmt.Errorf("aws_native: no DNS target configured for region %q", region)
		}
		changes = append(changes, r53types.Change{
			Action: r53types.ChangeActionUpsert,
			ResourceRecordSet: &r53types.ResourceRecordSet{
				Name:          aws.String(a.recordName),
				Type:          a.recordType,
				SetIdentifier: aws.String(region),
				Weight:        aws.Int64(int64(w[region])),
				TTL:           aws.Int64(recordTTL),
				ResourceRecords: []r53types.ResourceRecord{
					{Value: aws.String(target)},
				},
			},
		})
	}

	_, err := a.client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(a.hostedZone),
		ChangeBatch: &r53types.ChangeBatch{
			Comment: aws.String("health-checker-router weight update"),
			Changes: changes,
		},
	})
	if err != nil {
		return fmt.Errorf("aws_native: change resource record sets: %w", err)
	}
	return nil
}

// Failover sends all weight to one region.
func (a *Adapter) Failover(ctx context.Context, toRegion string) error {
	if _, ok := a.regionTargets[toRegion]; !ok {
		return fmt.Errorf("aws_native: unknown failover target region %q", toRegion)
	}
	w := routing.Weights{}
	for region := range a.regionTargets {
		w[region] = 0
	}
	w[toRegion] = 100
	return a.SetWeight(ctx, w)
}

// Status reads back the live weighted record sets.
func (a *Adapter) Status(ctx context.Context) (routing.ProviderStatus, error) {
	st := routing.ProviderStatus{Provider: models.ProviderAWSNative, Weights: routing.Weights{}}

	out, err := a.client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(a.hostedZone),
		StartRecordName: aws.String(a.recordName),
		StartRecordType: a.recordType,
	})
	if err != nil {
		st.Detail = err.Error()
		return st, fmt.Errorf("aws_native: list resource record sets: %w", err)
	}

	st.Reachable = true
	for _, rr := range out.ResourceRecordSets {
		if rr.Name == nil || rr.SetIdentifier == nil || rr.Weight == nil {
			continue
		}
		if normalizeName(*rr.Name) != normalizeName(a.recordName) {
			continue
		}
		st.Weights[*rr.SetIdentifier] = int(*rr.Weight)
	}
	st.Detail = fmt.Sprintf("hosted zone %s, record %s", a.hostedZone, a.recordName)
	return st, nil
}

// normalizeName strips the trailing dot Route 53 returns on FQDNs.
func normalizeName(n string) string {
	if len(n) > 0 && n[len(n)-1] == '.' {
		return n[:len(n)-1]
	}
	return n
}

// regionTargets reads the region -> DNS value map from config.
func regionTargets(ep models.RoutingEndpoint) (map[string]string, error) {
	raw, ok := ep.Config["regionTargets"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("aws_native: config field \"regionTargets\" is required " +
			"(map of region id to ALB/NLB hostname)")
	}
	out := make(map[string]string, len(raw))
	for region, v := range raw {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("aws_native: regionTargets[%s] must be a non-empty string", region)
		}
		out[region] = s
	}
	return out, nil
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
