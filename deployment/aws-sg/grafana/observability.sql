BEGIN;
SET LOCAL lock_timeout = '3s';

CREATE OR REPLACE FUNCTION grafana_sub2api.client_app(agent text)
RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
SELECT CASE
  WHEN agent ~* 'codex[ /_-]+desktop' THEN 'Codex Desktop'
  WHEN agent ~* 'codex[_ /-]+(cli|tui)|codex-cli-rs' THEN 'Codex CLI'
  WHEN agent ~* 'codex[_ /-]+(vscode|ide)' THEN 'Codex IDE'
  WHEN agent ~* '(^|[^a-z0-9])codex([^a-z0-9]|$)' THEN 'Codex'
  WHEN agent ~* 'claude[ /_-]+code|claude-cli' THEN 'Claude Code'
  WHEN agent ~* 'claude[ /_-]+desktop' THEN 'Claude Desktop'
  WHEN agent ~* 'opencode' THEN 'OpenCode'
  WHEN agent ~* 'cursor' THEN 'Cursor'
  WHEN agent ~* 'windsurf|codeium' THEN 'Windsurf'
  WHEN agent ~* 'cline' THEN 'Cline'
  WHEN agent ~* 'roo[ /_-]*code' THEN 'Roo Code'
  WHEN agent ~* 'aider' THEN 'Aider'
  WHEN agent ~* 'gemini[ /_-]+cli' THEN 'Gemini CLI'
  WHEN agent ~* 'cc[ /_-]+switch' THEN 'CC Switch'
  WHEN agent ~* 'openai|stainless' THEN 'OpenAI SDK'
  WHEN agent ~* 'anthropic' THEN 'Anthropic SDK'
  WHEN agent ~* 'curl/|postmanruntime|httpie|insomnia' THEN 'HTTP 工具'
  WHEN agent ~* 'python|axios|okhttp|go-http-client|node-fetch|undici' THEN '通用 HTTP 客户端'
  WHEN agent ~* 'mozilla|safari|chrome' THEN '浏览器'
  WHEN NULLIF(btrim(agent), '') IS NULL THEN '未知'
  ELSE '其他'
END
$$;

-- A bounded one-time import of existing CloudWatch access metadata. No raw log
-- line, query string, user IP, header, credential, or payload is stored here.
CREATE TABLE IF NOT EXISTS grafana_sub2api.gateway_access_history (
  event_id text PRIMARY KEY,
  created_at timestamptz NOT NULL,
  request_id text,
  client_request_id text,
  method text NOT NULL,
  endpoint text NOT NULL,
  status_code integer NOT NULL,
  elapsed_ms bigint NOT NULL
);
CREATE INDEX IF NOT EXISTS gateway_access_history_created_at ON grafana_sub2api.gateway_access_history (created_at);
CREATE INDEX IF NOT EXISTS gateway_access_history_request_id ON grafana_sub2api.gateway_access_history (request_id);
CREATE INDEX IF NOT EXISTS gateway_access_history_client_request_id ON grafana_sub2api.gateway_access_history (client_request_id);

CREATE OR REPLACE VIEW grafana_sub2api.gateway_access AS
SELECT created_at, request_id::text, client_request_id::text,
       coalesce(extra->>'method', '') AS method, coalesce(extra->>'path', '') AS endpoint,
       (extra->>'status_code')::integer AS status_code, (extra->>'latency_ms')::bigint AS elapsed_ms,
       '访问日志'::text AS timing_source
FROM public.ops_system_logs
WHERE message = 'http request completed'
  AND extra->>'status_code' ~ '^[0-9]{3}$' AND extra->>'latency_ms' ~ '^[0-9]{1,14}$'
UNION ALL
SELECT h.created_at, nullif(h.request_id, ''), nullif(h.client_request_id, ''), h.method, h.endpoint,
       h.status_code, h.elapsed_ms, 'CloudWatch 历史'::text
FROM grafana_sub2api.gateway_access_history h
WHERE h.created_at > now() - interval '30 days'
  AND NOT EXISTS (
    SELECT 1 FROM public.ops_system_logs s
    WHERE s.request_id = nullif(h.request_id, '') AND s.message = 'http request completed'
  );

CREATE OR REPLACE VIEW grafana_sub2api.request_records AS
WITH records AS (
  SELECT u.id, u.created_at, '成功'::text AS outcome, u.request_id::text,
    CASE WHEN u.request_id LIKE 'local:%' THEN substr(u.request_id, 7) END AS gateway_id,
    CASE WHEN u.request_id LIKE 'client:%' THEN substr(u.request_id, 8) END AS client_id,
    nullif(u.upstream_request_id, '')::text AS upstream_id, u.account_id, u.group_id, u.api_key_id,
    u.model::text, coalesce(nullif(u.requested_model, ''), u.model)::text AS requested_model,
    coalesce(nullif(u.upstream_model, ''), u.model)::text AS upstream_model,
    nullif(u.upstream_response_model, '')::text AS response_model,
    u.inbound_endpoint::text AS endpoint, u.upstream_endpoint::text, u.user_agent::text,
    u.reasoning_effort::text, u.service_tier::text, u.stream, u.request_type,
    null::integer AS gateway_status, null::integer AS upstream_status, null::text AS phase,
    null::text AS error_type, null::text AS provider_error, null::text AS network_error,
    null::text AS error_message, false AS business_limited,
    u.duration_ms::bigint AS forward_ms, u.first_token_ms::bigint AS ttft_ms,
    u.input_tokens, u.output_tokens, u.cache_read_tokens, u.cache_creation_tokens
  FROM public.usage_logs u
  UNION ALL
  SELECT e.id, e.created_at, '失败'::text, coalesce(nullif(e.request_id, ''), nullif(e.client_request_id, ''), '')::text,
    nullif(e.request_id, '')::text, nullif(e.client_request_id, '')::text, null::text, e.account_id, e.group_id, e.api_key_id,
    coalesce(nullif(e.model, ''), nullif(e.requested_model, ''))::text,
    coalesce(nullif(e.requested_model, ''), nullif(e.model, ''))::text, nullif(e.upstream_model, '')::text, null::text,
    coalesce(nullif(e.inbound_endpoint, ''), e.request_path)::text, e.upstream_endpoint::text, e.user_agent,
    null::text, null::text, e.stream, e.request_type,
    e.status_code, e.upstream_status_code, e.error_phase::text, e.error_type::text, e.provider_error_code::text, e.network_error_type::text,
    left(e.error_message, 480), e.is_business_limited,
    e.duration_ms::bigint, e.time_to_first_token_ms,
    null::integer, null::integer, null::integer, null::integer
  FROM public.ops_error_logs e
  WHERE e.status_code >= 400
)
SELECT r.id, r.created_at, r.outcome, r.request_id,
  coalesce(a.request_id, r.gateway_id) AS gateway_id,
  coalesce(r.client_id, a.client_request_id) AS client_id, r.upstream_id,
  r.account_id, r.group_id, r.api_key_id,
  coalesce(ac.name || ' #' || r.account_id, '#' || r.account_id, '未归属') AS account_label,
  coalesce(g.name || ' #' || r.group_id, '#' || r.group_id, '未归属') AS group_label,
  coalesce(k.name || ' #' || r.api_key_id, '#' || r.api_key_id, '未归属') AS key_label,
  coalesce(nullif(r.model, ''), '未知') AS model, r.requested_model, r.upstream_model, r.response_model,
  grafana_sub2api.client_app(r.user_agent) AS app,
  coalesce(nullif(a.endpoint, ''), r.endpoint) AS endpoint,
  r.upstream_endpoint, r.reasoning_effort, r.service_tier, r.stream, r.request_type,
  a.method, coalesce(r.gateway_status, a.status_code) AS gateway_status, a.status_code AS wire_status,
  r.upstream_status, r.phase, r.error_type, r.provider_error, r.network_error, r.error_message, r.business_limited,
  a.elapsed_ms AS e2e_ms, a.timing_source, r.forward_ms, r.ttft_ms,
  CASE WHEN r.outcome = '成功' AND (r.stream OR r.request_type IN (2,3)) AND r.ttft_ms > 0
        AND r.forward_ms > r.ttft_ms AND r.output_tokens > 1
       THEN (r.forward_ms - r.ttft_ms)::double precision / (r.output_tokens - 1) END AS tpot_ms,
  r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_creation_tokens
FROM records r
LEFT JOIN LATERAL (
  SELECT a.* FROM grafana_sub2api.gateway_access a
  WHERE a.created_at BETWEEN r.created_at - interval '3 minutes' AND r.created_at + interval '3 minutes'
    AND ((r.gateway_id IS NOT NULL AND a.request_id = r.gateway_id)
      OR (r.client_id IS NOT NULL AND a.client_request_id = r.client_id))
  ORDER BY (a.request_id = r.gateway_id) DESC NULLS LAST,
    abs(extract(epoch FROM a.created_at - r.created_at)),
    CASE WHEN a.timing_source = '访问日志' THEN 0 ELSE 1 END
  LIMIT 1
) a ON true
LEFT JOIN public.accounts ac ON ac.id = r.account_id
LEFT JOIN public.groups g ON g.id = r.group_id
LEFT JOIN public.api_keys k ON k.id = r.api_key_id;

CREATE OR REPLACE VIEW grafana_sub2api.access_records AS
SELECT a.created_at, coalesce(a.request_id, a.client_request_id, '') AS request_id,
       a.request_id AS gateway_id, a.client_request_id AS client_id, null::text AS upstream_id,
       r.account_id, r.group_id, r.api_key_id, coalesce(r.account_label, '未归属') AS account_label,
       coalesce(r.group_label, '未归属') AS group_label, coalesce(r.key_label, '未归属') AS key_label,
       coalesce(r.model, '未知') AS model, coalesce(r.app, '未知') AS app, a.method, a.endpoint,
       a.status_code AS gateway_status, null::integer AS upstream_status, null::text AS provider_error,
       a.elapsed_ms AS e2e_ms, a.timing_source, r.id IS NOT NULL AS matched
FROM grafana_sub2api.gateway_access a
LEFT JOIN LATERAL (
  SELECT r.id, r.account_id, r.group_id, r.api_key_id, r.account_label, r.group_label, r.key_label, r.model, r.app
  FROM grafana_sub2api.request_records r
  WHERE r.created_at BETWEEN a.created_at - interval '3 minutes' AND a.created_at + interval '3 minutes'
    AND ((a.request_id IS NOT NULL AND r.request_id IN (a.request_id, 'local:' || a.request_id))
      OR (a.client_request_id IS NOT NULL AND r.request_id IN (a.client_request_id, 'client:' || a.client_request_id)))
  ORDER BY abs(extract(epoch FROM r.created_at - a.created_at))
  LIMIT 1
) r ON true
WHERE a.endpoint ~ '^/(v1(beta)?/|anthropic/|openai/|gemini/)';

CREATE OR REPLACE VIEW grafana_sub2api.traffic_records AS
SELECT created_at, outcome, account_id, group_id, api_key_id, account_label, group_label, key_label,
       model, app, gateway_status, upstream_status, provider_error, error_type, ttft_ms, tpot_ms, e2e_ms,
       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens
FROM grafana_sub2api.request_records
UNION ALL
SELECT created_at, CASE WHEN gateway_status >= 400 THEN '失败' ELSE '仅访问日志' END,
       account_id, group_id, api_key_id, account_label, group_label, key_label, model, app,
       gateway_status, upstream_status, provider_error, 'HTTP 访问日志', null::bigint, null::double precision, e2e_ms,
       null::integer, null::integer, null::integer, null::integer
FROM grafana_sub2api.access_records WHERE NOT matched;

CREATE OR REPLACE VIEW grafana_sub2api.egress_records AS
WITH attempts AS (
  SELECT e.id, e.created_at, e.request_id, e.client_request_id, e.group_id, e.api_key_id,
    e.model, e.requested_model, e.upstream_model, e.upstream_endpoint, e.user_agent,
    e.status_code, e.provider_error_code, e.network_error_type, e.upstream_latency_ms,
    CASE WHEN v.event->>'account_id' ~ '^[0-9]{1,18}$' AND v.event->>'account_id' <> '0'
         THEN (v.event->>'account_id')::bigint ELSE e.account_id END AS account_id,
    v.position, v.event
  FROM public.ops_error_logs e
  CROSS JOIN LATERAL jsonb_array_elements(
    CASE WHEN CASE WHEN jsonb_typeof(e.upstream_errors) = 'array' THEN jsonb_array_length(e.upstream_errors) > 0 ELSE false END
         THEN e.upstream_errors
         WHEN e.upstream_status_code IS NOT NULL OR e.upstream_latency_ms IS NOT NULL
         THEN jsonb_build_array(jsonb_build_object('kind', 'terminal_upstream_result',
           'upstream_status_code', e.upstream_status_code, 'message', e.upstream_error_message))
         ELSE '[]'::jsonb END
  ) WITH ORDINALITY v(event, position)
  WHERE jsonb_typeof(v.event) = 'object'
)
SELECT u.created_at AS recorded_at, u.created_at AS event_at, u.request_id, u.gateway_id, u.client_id, u.upstream_id,
       u.account_id, u.group_id, u.api_key_id, u.account_label, u.group_label, u.key_label, u.app, u.model,
       u.requested_model, u.upstream_model, u.response_model, u.upstream_endpoint,
       null::text AS upstream_url, null::text AS proxy, '成功'::text AS outcome, '用量记录'::text AS source,
       null::bigint AS attempt, u.gateway_status, null::integer AS upstream_status, null::text AS provider_error,
       null::text AS error_kind, null::text AS error_stage, null::text AS result_summary,
       null::bigint AS headers_ms, u.forward_ms, u.ttft_ms, u.reasoning_effort, u.service_tier,
       u.input_tokens, u.output_tokens, u.cache_read_tokens, u.cache_creation_tokens
FROM grafana_sub2api.request_records u
WHERE u.outcome = '成功' AND u.account_id IS NOT NULL AND nullif(u.upstream_endpoint, '') IS NOT NULL
UNION ALL
SELECT a.created_at,
       CASE WHEN a.event->>'at_unix_ms' ~ '^[1-9][0-9]{12}$' THEN to_timestamp((a.event->>'at_unix_ms')::double precision / 1000)
            ELSE a.created_at END,
       coalesce(nullif(a.request_id, ''), a.client_request_id, '')::text, a.request_id::text, a.client_request_id::text,
       a.event->>'upstream_request_id',
       a.account_id, a.group_id, a.api_key_id,
       coalesce(ac.name || ' #' || a.account_id, '#' || a.account_id, '未归属'),
       coalesce(g.name || ' #' || a.group_id, '#' || a.group_id, '未归属'),
       coalesce(k.name || ' #' || a.api_key_id, '#' || a.api_key_id, '未归属'),
       grafana_sub2api.client_app(a.user_agent), coalesce(nullif(a.model, ''), nullif(a.requested_model, ''), '未知'),
       coalesce(nullif(a.requested_model, ''), a.model)::text, nullif(a.upstream_model, '')::text, null::text,
       a.upstream_endpoint::text, left(a.event->>'upstream_url', 512), a.event->>'proxy_name',
       '失败尝试'::text, '上游错误事件'::text, a.position, a.status_code,
       CASE WHEN a.event->>'upstream_status_code' ~ '^[1-5][0-9]{2}$' THEN (a.event->>'upstream_status_code')::integer END,
       coalesce(a.event->>'provider_error_code',
                CASE WHEN a.event->>'kind' = 'terminal_upstream_result' THEN a.provider_error_code END),
       coalesce(a.event->>'kind', a.network_error_type), a.event->>'stage',
       left(coalesce(nullif(a.event->>'reason', ''), nullif(a.event->>'message', '')), 480),
       CASE WHEN a.event->>'kind' = 'terminal_upstream_result' THEN a.upstream_latency_ms END,
       null::bigint, null::bigint, null::text, null::text, null::integer, null::integer, null::integer, null::integer
FROM attempts a
LEFT JOIN public.accounts ac ON ac.id = a.account_id
LEFT JOIN public.groups g ON g.id = a.group_id
LEFT JOIN public.api_keys k ON k.id = a.api_key_id;

CREATE OR REPLACE VIEW grafana_sub2api.diagnostic_records AS
SELECT s.id, s.created_at, s.level, s.component, left(s.message, 480) AS message,
  coalesce(nullif(s.request_id, ''), nullif(s.client_request_id, ''), '')::text AS request_id,
  nullif(s.client_request_id, '')::text AS client_id,
  coalesce(s.account_id, r.account_id) AS account_id, r.group_id, coalesce(s.api_key_id, r.api_key_id) AS api_key_id,
  coalesce(ac.name || ' #' || coalesce(s.account_id, r.account_id), '#' || coalesce(s.account_id, r.account_id), '未归属') AS account_label,
  coalesce(g.name || ' #' || r.group_id, '#' || r.group_id, '未归属') AS group_label,
  coalesce(k.name || ' #' || coalesce(s.api_key_id, r.api_key_id), '#' || coalesce(s.api_key_id, r.api_key_id), '未归属') AS key_label,
  coalesce(nullif(s.model, ''), r.model, '未知') AS model, coalesce(r.app, '未知') AS app,
  coalesce(s.extra->>'path', r.endpoint) AS endpoint
FROM public.ops_system_logs s
LEFT JOIN LATERAL (
  SELECT r.account_id, r.group_id, r.api_key_id, r.model, r.app, r.endpoint
  FROM grafana_sub2api.request_records r
  WHERE r.created_at BETWEEN s.created_at - interval '3 minutes' AND s.created_at + interval '3 minutes'
    AND (s.api_key_id IS NULL OR r.api_key_id = s.api_key_id)
    AND ((nullif(s.request_id, '') IS NOT NULL AND (r.request_id IN (s.request_id, 'local:' || s.request_id)))
      OR (nullif(s.client_request_id, '') IS NOT NULL AND r.request_id IN (s.client_request_id, 'client:' || s.client_request_id)))
  ORDER BY abs(extract(epoch FROM r.created_at - s.created_at))
  LIMIT 1
) r ON true
LEFT JOIN public.accounts ac ON ac.id = coalesce(s.account_id, r.account_id)
LEFT JOIN public.groups g ON g.id = r.group_id
LEFT JOIN public.api_keys k ON k.id = coalesce(s.api_key_id, r.api_key_id)
WHERE s.message <> 'http request completed';

REVOKE ALL ON FUNCTION grafana_sub2api.client_app(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION grafana_sub2api.client_app(text) TO grafana_sub2api_reader;
GRANT SELECT ON grafana_sub2api.request_records, grafana_sub2api.egress_records,
  grafana_sub2api.diagnostic_records, grafana_sub2api.access_records,
  grafana_sub2api.traffic_records TO grafana_sub2api_reader;

-- The app refreshes its Ops snapshot periodically. Preserve all other fields.
INSERT INTO public.settings (key, value, updated_at)
VALUES ('ops_runtime_log_config', '{"persist_access_logs":true}', now())
ON CONFLICT (key) DO UPDATE
SET value = jsonb_set(public.settings.value::jsonb, '{persist_access_logs}', 'true'::jsonb, true)::text,
    updated_at = now()
WHERE public.settings.value::jsonb->>'persist_access_logs' IS DISTINCT FROM 'true';
COMMIT;
