// Package awsauth resolves AWS credentials for probes and routing adapters.
//
// Four methods are supported, matching the Admin form. In every case what is
// stored is a *reference* — a role ARN, a secret path, an SSO profile, a binary
// path — never a credential. Values are resolved here, at call time, and cached
// only in memory.
package awsauth

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/beravelli/health-checker-router/internal/config"
	"github.com/beravelli/health-checker-router/internal/models"
)

// Spec describes how to obtain credentials for one target.
type Spec struct {
	Method models.AWSAuthMethod
	// Ref is the role ARN, secret path, SSO profile name, or binary path,
	// depending on Method.
	Ref string
	// ExternalID is optional and only used with AuthAssumeRole.
	ExternalID string
	// Region is the AWS region the resulting client should target.
	Region string
	// BinaryArgs are passed to the custom SSO binary.
	BinaryArgs []string
}

// cacheKey identifies a resolved config so short-lived credentials are reused
// until they near expiry, instead of re-assuming a role on every 5s probe.
func (s Spec) cacheKey() string {
	return string(s.Method) + "|" + s.Ref + "|" + s.Region + "|" + s.ExternalID
}

type cached struct {
	cfg       aws.Config
	expiresAt time.Time
}

// Resolver builds and caches AWS configs.
type Resolver struct {
	appCfg *config.Config

	mu    sync.Mutex
	items map[string]cached
	// ttl bounds how long a resolved config is reused. The SDK refreshes
	// credentials internally; this only bounds config reuse.
	ttl time.Duration
}

// NewResolver returns a Resolver with a 15 minute config cache.
func NewResolver(appCfg *config.Config) *Resolver {
	return &Resolver{appCfg: appCfg, items: map[string]cached{}, ttl: 15 * time.Minute}
}

// Config returns an aws.Config for the given spec.
func (r *Resolver) Config(ctx context.Context, spec Spec) (aws.Config, error) {
	key := spec.cacheKey()

	r.mu.Lock()
	if c, ok := r.items[key]; ok && time.Now().Before(c.expiresAt) {
		r.mu.Unlock()
		return c.cfg, nil
	}
	r.mu.Unlock()

	cfg, err := r.build(ctx, spec)
	if err != nil {
		return aws.Config{}, err
	}

	r.mu.Lock()
	r.items[key] = cached{cfg: cfg, expiresAt: time.Now().Add(r.ttl)}
	r.mu.Unlock()
	return cfg, nil
}

func (r *Resolver) build(ctx context.Context, spec Spec) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if spec.Region != "" {
		opts = append(opts, awsconfig.WithRegion(spec.Region))
	}

	switch spec.Method {
	case models.AuthAssumeRole:
		if spec.Ref == "" {
			return aws.Config{}, fmt.Errorf("assume_role requires a role ARN")
		}
		// Base config uses the pod's own identity (IRSA, instance profile),
		// which is then used to assume the target role.
		base, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return aws.Config{}, fmt.Errorf("load base config: %w", err)
		}
		stsClient := sts.NewFromConfig(base)
		provider := stscreds.NewAssumeRoleProvider(stsClient, spec.Ref, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = "health-checker-router"
			if spec.ExternalID != "" {
				o.ExternalID = aws.String(spec.ExternalID)
			}
			// Short sessions, refreshed by the SDK before expiry.
			o.Duration = 15 * time.Minute
		})
		base.Credentials = aws.NewCredentialsCache(provider)
		return base, nil

	case models.AuthServiceAccount:
		// Long-lived-but-rotated credentials fetched from the secrets engine.
		// spec.Ref names an env var holding "AKID:SECRET" or a JSON blob; the
		// injector (Vault Agent, ESO, CSI driver) populates it.
		raw, err := config.Resolve(spec.Ref)
		if err != nil {
			return aws.Config{}, fmt.Errorf("resolve service account credentials: %w", err)
		}
		id, secret, token, err := parseStaticCreds(raw)
		if err != nil {
			return aws.Config{}, err
		}
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(id, secret, token)))
		cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return aws.Config{}, fmt.Errorf("load config with static credentials: %w", err)
		}
		return cfg, nil

	case models.AuthAWSSSO:
		// The SDK reads the cached SSO token written by `aws sso login`. The
		// password never reaches this process; only the short-lived session
		// token is used, and expiry surfaces as an auth error the operator
		// resolves by logging in again.
		if spec.Ref != "" {
			opts = append(opts, awsconfig.WithSharedConfigProfile(spec.Ref))
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return aws.Config{}, fmt.Errorf("load SSO profile %q: %w", spec.Ref, err)
		}
		return cfg, nil

	case models.AuthCustomSSOBinary:
		// Orgs with an internal SSO wrapper plug in here. The contract is the
		// same as the AWS CLI's credential_process: the binary prints JSON
		// credentials to stdout.
		if !r.appCfg.BinaryAllowed(spec.Ref) {
			return aws.Config{}, fmt.Errorf(
				"custom SSO binary %q is not on SSO_BINARY_ALLOWLIST", spec.Ref)
		}
		provider := &processCredentials{path: spec.Ref, args: spec.BinaryArgs}
		opts = append(opts, awsconfig.WithCredentialsProvider(aws.NewCredentialsCache(provider)))
		cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return aws.Config{}, fmt.Errorf("load config with credential process: %w", err)
		}
		return cfg, nil

	case "":
		// No method configured: fall back to the pod's ambient identity.
		cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return aws.Config{}, fmt.Errorf("load default config: %w", err)
		}
		return cfg, nil

	default:
		return aws.Config{}, fmt.Errorf("unsupported AWS auth method %q", spec.Method)
	}
}

// parseStaticCreds accepts either "AKID:SECRET[:TOKEN]" or the JSON shape the
// AWS CLI credential_process emits.
func parseStaticCreds(raw string) (id, secret, token string, err error) {
	raw = strings.TrimSpace(raw)

	if strings.HasPrefix(raw, "{") {
		var p processOutput
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return "", "", "", fmt.Errorf("decode JSON credentials: %w", err)
		}
		if p.AccessKeyID == "" || p.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("JSON credentials missing AccessKeyId or SecretAccessKey")
		}
		return p.AccessKeyID, p.SecretAccessKey, p.SessionToken, nil
	}

	parts := strings.Split(raw, ":")
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("credentials must be JSON or AKID:SECRET[:TOKEN]")
	}
	if len(parts) > 2 {
		token = parts[2]
	}
	return parts[0], parts[1], token, nil
}

// processOutput is the credential_process JSON contract.
type processOutput struct {
	Version         int        `json:"Version"`
	AccessKeyID     string     `json:"AccessKeyId"`
	SecretAccessKey string     `json:"SecretAccessKey"`
	SessionToken    string     `json:"SessionToken"`
	Expiration      *time.Time `json:"Expiration"`
}

// processCredentials runs an external binary to obtain credentials.
type processCredentials struct {
	path string
	args []string
}

// Retrieve implements aws.CredentialsProvider.
func (p *processCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, p.path, p.args...).Output()
	if err != nil {
		// Deliberately does not include stderr in the error: an SSO wrapper
		// may echo sensitive context there.
		return aws.Credentials{}, fmt.Errorf("credential process %q failed: %w", p.path, err)
	}

	var parsed processOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return aws.Credentials{}, fmt.Errorf("credential process %q did not emit valid JSON: %w", p.path, err)
	}
	if parsed.AccessKeyID == "" || parsed.SecretAccessKey == "" {
		return aws.Credentials{}, fmt.Errorf("credential process %q returned incomplete credentials", p.path)
	}

	creds := aws.Credentials{
		AccessKeyID:     parsed.AccessKeyID,
		SecretAccessKey: parsed.SecretAccessKey,
		SessionToken:    parsed.SessionToken,
		Source:          "health-checker-router/credential-process",
	}
	if parsed.Expiration != nil {
		creds.CanExpire = true
		creds.Expires = *parsed.Expiration
	}
	return creds, nil
}

// SpecFromService builds a Spec from a stored service definition.
func SpecFromService(svc models.Service, region string) Spec {
	return Spec{
		Method:     svc.AWSAuthMethod,
		Ref:        svc.AWSAuthRef,
		ExternalID: stringField(svc.ProbeConfig, "awsExternalId"),
		Region:     region,
		BinaryArgs: stringsField(svc.ProbeConfig, "ssoBinaryArgs"),
	}
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func stringsField(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
