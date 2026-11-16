BEGIN;
SET LOCAL lock_timeout='3s';

CREATE TABLE IF NOT EXISTS grafana_sub2api.egress_payloads (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    gateway_id text,
    client_id text,
    api_key_id bigint NOT NULL,
    account_id bigint NOT NULL,
    method text NOT NULL,
    url text NOT NULL,
    status_code integer,
    failure text,
    request_json jsonb NOT NULL,
    response_json jsonb NOT NULL,
    CHECK (gateway_id IS NOT NULL OR client_id IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS egress_payloads_gateway ON grafana_sub2api.egress_payloads (gateway_id, created_at DESC);
CREATE INDEX IF NOT EXISTS egress_payloads_client ON grafana_sub2api.egress_payloads (client_id, created_at DESC);
CREATE INDEX IF NOT EXISTS egress_payloads_expiry ON grafana_sub2api.egress_payloads (expires_at);

CREATE OR REPLACE VIEW grafana_sub2api.egress_payload_details AS
SELECT p.id, p.created_at, p.gateway_id, p.client_id, p.api_key_id, p.account_id, p.method, p.url,
       p.status_code, p.failure, p.request_json, p.response_json,
       coalesce(k.name || ' #' || p.api_key_id, '#' || p.api_key_id) AS key_label,
       coalesce(a.name || ' #' || p.account_id, '#' || p.account_id) AS account_label
FROM grafana_sub2api.egress_payloads p
LEFT JOIN public.api_keys k ON k.id = p.api_key_id
LEFT JOIN public.accounts a ON a.id = p.account_id
WHERE p.expires_at > now();

REVOKE ALL ON grafana_sub2api.egress_payloads FROM PUBLIC;
GRANT SELECT ON grafana_sub2api.egress_payload_details TO grafana_sub2api_reader;
COMMIT;
