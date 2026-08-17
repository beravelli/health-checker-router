// Package cloudfrontadapter routes traffic via a CloudFront origin group.
//
// CloudFront has no weighted split — an origin group is primary/secondary with
// automatic failover on configured status codes. So SetWeight cannot express an
// arbitrary ratio: it picks whichever region holds majority weight and makes
// that the primary origin. This is a real limitation of the provider, surfaced
// honestly rather than silently approximated.
package cloudfrontadapter

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"

	"github.com/beravelli/health-checker-router/internal/awsauth"
	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/routing"
)

// Adapter implements routing.Provider against CloudFront.
type Adapter struct {
	client         *cloudfront.Client
	distributionID string
	originGroupID  string
	// regionOrigins maps region id to the CloudFront origin id serving it.
	regionOrigins map[string]string
}

// Factory builds an Adapter from stored configuration.
func Factory(resolver *awsauth.Resolver) routing.Factory {
	return func(ctx context.Context, ep models.RoutingEndpoint) (routing.Provider, error) {
		dist, err := routing.ConfigString(ep, "distributionId", true)
		if err != nil {
			return nil, err
		}
		group, err := routing.ConfigString(ep, "originGroupId", true)
		if err != nil {
			return nil, err
		}

		origins, err := regionOrigins(ep)
		if err != nil {
			return nil, err
		}

		cfg, err := resolver.Config(ctx, awsauth.Spec{
			Method:     models.AWSAuthMethod(ep.AuthMethod),
			Ref:        ep.AuthRef,
			ExternalID: stringField(ep.Config, "externalId"),
			Region:     "us-east-1", // CloudFront's API is global, served from us-east-1
		})
		if err != nil {
			return nil, fmt.Errorf("cloudfront: resolve credentials: %w", err)
		}

		return &Adapter{
			client:         cloudfront.NewFromConfig(cfg),
			distributionID: dist,
			originGroupID:  group,
			regionOrigins:  origins,
		}, nil
	}
}

// Name identifies the provider.
func (a *Adapter) Name() models.RoutingProvider { return models.ProviderCloudFront }

// SetWeight promotes the majority-weight region to primary origin.
func (a *Adapter) SetWeight(ctx context.Context, w routing.Weights) error {
	if err := w.Validate(); err != nil {
		return fmt.Errorf("cloudfront: %w", err)
	}

	primary, secondary := "", ""
	best := -1
	for _, region := range w.Regions() {
		if w[region] > best {
			if primary != "" {
				secondary = primary
			}
			primary, best = region, w[region]
		} else if secondary == "" {
			secondary = region
		}
	}
	if primary == "" {
		return fmt.Errorf("cloudfront: could not determine a primary region from weights")
	}
	return a.setOriginOrder(ctx, primary, secondary)
}

// Failover promotes a region to primary origin immediately.
func (a *Adapter) Failover(ctx context.Context, toRegion string) error {
	if _, ok := a.regionOrigins[toRegion]; !ok {
		return fmt.Errorf("cloudfront: unknown failover target region %q", toRegion)
	}
	secondary := ""
	for region := range a.regionOrigins {
		if region != toRegion {
			secondary = region
			break
		}
	}
	return a.setOriginOrder(ctx, toRegion, secondary)
}

// setOriginOrder rewrites the origin group's failover order.
//
// CloudFront requires a read-modify-write against the whole distribution
// config, guarded by the ETag, so a concurrent edit fails rather than silently
// clobbering unrelated settings.
func (a *Adapter) setOriginOrder(ctx context.Context, primary, secondary string) error {
	primaryOrigin, ok := a.regionOrigins[primary]
	if !ok {
		return fmt.Errorf("cloudfront: no origin configured for region %q", primary)
	}

	current, err := a.client.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{
		Id: aws.String(a.distributionID),
	})
	if err != nil {
		return fmt.Errorf("cloudfront: get distribution config: %w", err)
	}
	if current.DistributionConfig == nil || current.ETag == nil {
		return fmt.Errorf("cloudfront: distribution %s returned an empty config", a.distributionID)
	}

	members := []cftypes.OriginGroupMember{{OriginId: aws.String(primaryOrigin)}}
	if secondaryOrigin, ok := a.regionOrigins[secondary]; ok && secondary != primary {
		members = append(members, cftypes.OriginGroupMember{OriginId: aws.String(secondaryOrigin)})
	}

	cfg := current.DistributionConfig
	if cfg.OriginGroups == nil || cfg.OriginGroups.Items == nil {
		return fmt.Errorf("cloudfront: distribution %s has no origin groups", a.distributionID)
	}

	found := false
	for i := range cfg.OriginGroups.Items {
		g := &cfg.OriginGroups.Items[i]
		if g.Id == nil || *g.Id != a.originGroupID {
			continue
		}
		g.Members = &cftypes.OriginGroupMembers{
			Items:    members,
			Quantity: aws.Int32(int32(len(members))),
		}
		found = true
		break
	}
	if !found {
		return fmt.Errorf("cloudfront: origin group %q not found in distribution %s",
			a.originGroupID, a.distributionID)
	}

	if _, err := a.client.UpdateDistribution(ctx, &cloudfront.UpdateDistributionInput{
		Id:                 aws.String(a.distributionID),
		IfMatch:            current.ETag,
		DistributionConfig: cfg,
	}); err != nil {
		return fmt.Errorf("cloudfront: update distribution: %w", err)
	}
	return nil
}

// Status reports the current primary origin as a 100/0 split, since CloudFront
// origin groups cannot express a ratio.
func (a *Adapter) Status(ctx context.Context) (routing.ProviderStatus, error) {
	st := routing.ProviderStatus{Provider: models.ProviderCloudFront, Weights: routing.Weights{}}

	current, err := a.client.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{
		Id: aws.String(a.distributionID),
	})
	if err != nil {
		st.Detail = err.Error()
		return st, fmt.Errorf("cloudfront: get distribution config: %w", err)
	}
	st.Reachable = true

	originToRegion := map[string]string{}
	for region, origin := range a.regionOrigins {
		originToRegion[origin] = region
		st.Weights[region] = 0
	}

	cfg := current.DistributionConfig
	if cfg != nil && cfg.OriginGroups != nil {
		for _, g := range cfg.OriginGroups.Items {
			if g.Id == nil || *g.Id != a.originGroupID || g.Members == nil {
				continue
			}
			if len(g.Members.Items) > 0 && g.Members.Items[0].OriginId != nil {
				if region, ok := originToRegion[*g.Members.Items[0].OriginId]; ok {
					st.Weights[region] = 100
				}
			}
		}
	}
	st.Detail = fmt.Sprintf("distribution %s, origin group %s (primary/secondary failover, no weighted split)",
		a.distributionID, a.originGroupID)
	return st, nil
}

func regionOrigins(ep models.RoutingEndpoint) (map[string]string, error) {
	raw, ok := ep.Config["regionOrigins"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("cloudfront: config field \"regionOrigins\" is required " +
			"(map of region id to CloudFront origin id)")
	}
	out := make(map[string]string, len(raw))
	for region, v := range raw {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("cloudfront: regionOrigins[%s] must be a non-empty string", region)
		}
		out[region] = s
	}
	return out, nil
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
