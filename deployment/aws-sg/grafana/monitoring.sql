BEGIN;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'grafana_sub2api_reader') THEN
        CREATE ROLE grafana_sub2api_reader LOGIN;
    END IF;
END $$;
ALTER ROLE grafana_sub2api_reader PASSWORD __GRAFANA_READER_VERIFIER__;
ALTER ROLE grafana_sub2api_reader SET default_transaction_read_only = on;
ALTER ROLE grafana_sub2api_reader SET statement_timeout = '8s';
ALTER ROLE grafana_sub2api_reader SET lock_timeout = '2s';
ALTER ROLE grafana_sub2api_reader CONNECTION LIMIT 5;

SET LOCAL ROLE sub2api;
CREATE SCHEMA IF NOT EXISTS grafana_sub2api;
REVOKE ALL ON SCHEMA grafana_sub2api FROM PUBLIC;

CREATE OR REPLACE VIEW grafana_sub2api.requests AS
SELECT created_at, request_id, model, requested_model, upstream_model,
       inbound_endpoint, reasoning_effort, duration_ms, first_token_ms,
       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
       upstream_request_id
FROM public.usage_logs;

CREATE OR REPLACE VIEW grafana_sub2api.errors AS
SELECT created_at, request_id, client_request_id, model, request_path,
       error_phase, error_type, status_code, upstream_status_code,
       is_business_limited
FROM public.ops_error_logs;

GRANT CONNECT ON DATABASE sub2api TO grafana_sub2api_reader;
GRANT USAGE ON SCHEMA grafana_sub2api TO grafana_sub2api_reader;
GRANT SELECT ON grafana_sub2api.requests, grafana_sub2api.errors TO grafana_sub2api_reader;
COMMIT;
