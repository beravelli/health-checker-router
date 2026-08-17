-- +goose Up
-- +goose StatementBegin

-- Seeds the two active-active regions and their default 50/50 split, plus the
-- four routing providers as disabled placeholders so Admin has rows to edit.
-- Safe to re-run: everything is ON CONFLICT DO NOTHING.

INSERT INTO regions (id, display_name, provider, zone_label) VALUES
  ('us-east-1', 'us-east-1', 'AWS', 'primary write region'),
  ('us-west-2', 'us-west-2', 'AWS', 'secondary write region')
ON CONFLICT (id) DO NOTHING;

INSERT INTO lb_weights (region_id, weight, is_automatic) VALUES
  ('us-east-1', 50, true),
  ('us-west-2', 50, true)
ON CONFLICT (region_id) DO NOTHING;

INSERT INTO routing_endpoints (provider, enabled, config) VALUES
  ('aws_native',      false, '{}'),
  ('cloudfront',      false, '{}'),
  ('avi_gslb',        false, '{}'),
  ('akamai_cloudlet', false, '{}')
ON CONFLICT (provider) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM routing_endpoints;
DELETE FROM lb_weights WHERE region_id IN ('us-east-1', 'us-west-2');
DELETE FROM regions    WHERE id        IN ('us-east-1', 'us-west-2');
-- +goose StatementEnd
