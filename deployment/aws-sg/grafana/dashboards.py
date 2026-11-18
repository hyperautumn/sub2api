import argparse
import json
from pathlib import Path


REGION = "ap-southeast-1"
CLOUDWATCH = {"type": "cloudwatch", "uid": "sub2api-cloudwatch"}
POSTGRES = {"type": "grafana-postgresql-datasource", "uid": "sub2api-postgres"}
METRIC_DECIMALS = 2


def panel(title, source, targets, kind="timeseries", unit="short", description=""):
    defaults = {"unit": unit}
    if kind == "timeseries":
        defaults["decimals"] = METRIC_DECIMALS
    return {
        "title": title,
        "description": description,
        "type": kind,
        "datasource": source,
        "fieldConfig": {"defaults": defaults, "overrides": []},
        "targets": [dict(t, refId=chr(65 + i), datasource=source) for i, t in enumerate(targets)],
    }


def dashboard(uid, title, panels, variables=(), hours=6, split=False):
    for i, item in enumerate(panels):
        item["id"] = i + 1
        w = 12 if split else 24
        h = 8 if split else 11
        item["gridPos"] = {"x": i % 2 * w if split else 0, "y": (i // 2 if split else i) * h, "w": w, "h": h}
    return {
        "uid": uid, "title": title, "schemaVersion": 39, "version": 1,
        "timezone": "Asia/Singapore", "tags": ["sub2api"], "editable": False,
        "time": {"from": f"now-{hours}h", "to": "now"}, "refresh": "",
        "templating": {"list": [{"name": k, "label": v, "type": "textbox", "query": "", "current": {"text": "", "value": ""}} for k, v in variables]},
        "panels": panels,
    }


def sql(query, fmt="time_series"):
    return {"editorMode": "code", "format": fmt, "rawQuery": True, "rawSql": query}


SCHEMA = "grafana_sub2api"
ALL = "__all__"
UNKNOWN = "__unattributed__"
DIMENSIONS = (
    ("account", "上游账号", "account_id::text", "account_label"),
    ("model", "模型", "model", "model"),
    ("group", "分组", "group_id::text", "group_label"),
    ("api_key", "API Key", "api_key_id::text", "key_label"),
    ("app", "应用", "app", "app"),
)
REQUESTS = f"{SCHEMA}.request_records"
ACCESS = f"{SCHEMA}.access_records"
TRAFFIC = f"{SCHEMA}.traffic_records"
EGRESS = f"{SCHEMA}.egress_records"
DIAGNOSTICS = f"{SCHEMA}.diagnostic_records"
TPM_UNIT = "M TPM"
TOKENS_PER_MILLION = 1_000_000
DASHBOARDS = (
    ("sub2api-traffic", "流量指标"),
    ("sub2api-logs", "全局日志"),
    ("sub2api-egress", "上游出口"),
    ("sub2api-aws", "AWS 基础设施"),
)


def textbox(name, label):
    return {"name": name, "label": label, "type": "textbox", "query": "",
            "current": {"text": "", "value": ""}, "options": []}


def choice(name, label, choices, default):
    return {
        "name": name, "label": label, "type": "custom",
        "query": ",".join(f"{text} : {value}" for value, text in choices),
        "current": {"text": dict(choices)[default], "value": default},
        "options": [
            {"text": text, "value": value, "selected": value == default} for value, text in choices
        ],
    }


def dimension_variables():
    variables = []
    for name, label, value, label_column in DIMENSIONS:
        query = f"""SELECT label AS __text, value AS __value
FROM (
  SELECT DISTINCT {label_column} AS label, coalesce({value}, '{UNKNOWN}') AS value
  FROM {TRAFFIC} WHERE $__timeFilter(created_at)
  UNION
  SELECT DISTINCT {label_column}, coalesce({value}, '{UNKNOWN}')
  FROM {EGRESS} WHERE $__timeFilter(recorded_at)
  UNION
  SELECT '{'未归属' if '_id' in value else '未知'}', '{UNKNOWN if '_id' in value else '未知'}'
) labels ORDER BY label"""
        variables.append({
            "name": name, "label": label, "type": "query", "datasource": POSTGRES,
            "query": query, "definition": query, "refresh": 2, "multi": True,
            "includeAll": True, "allValue": f"'{ALL}'",
            "current": {"text": "All", "value": "$__all"}, "options": [],
        })
    return variables


def common_filters(time_column="created_at", request_id=False):
    conditions = [f"$__timeFilter({time_column})"]
    for name, _, column, _ in DIMENSIONS:
        vals = "${" + name + ":sqlstring}"
        conditions.append(f"('{ALL}' IN ({vals}) OR coalesce({column}, '{UNKNOWN}') IN ({vals}))")
    if request_id:
        request = "${request_id:sqlstring}"
        ids = ["request_id", "client_id", "'client:' || client_id"]
        if request_id == "full":
            ids.extend(["gateway_id", "'local:' || gateway_id", "upstream_id"])
        ids_sql = ", ".join(ids)
        conditions.append(f"({request} = '' OR {request} IN ({ids_sql}))")
    return "\n  AND ".join(conditions)


def status_filter():
    return """(${status:sqlstring} = '' OR ${status:sqlstring} IN (gateway_status::text, upstream_status::text, provider_error))"""


def path_filter(field="endpoint"):
    return f"(${{path:sqlstring}} = '' OR {field} = ${{path:sqlstring}})"


def layout_dashboard(uid, title, panels, variables, hours=3):
    layout = dashboard(uid, "Sub2API " + title, [], hours=hours)
    y = 0
    panel_id = 0
    for row in panels:
        width = 24 // len(row)
        height = 11 if len(row) == 1 else 8
        for i, item in enumerate(row):
            panel_id += 1
            item.update(id=panel_id, gridPos={"x": i * width, "y": y, "w": width, "h": height})
            layout["panels"].append(item)
        y += height
    layout["templating"]["list"] = variables
    layout["links"] = [
        {"title": name, "type": "link", "url": f"/d/{target}", "includeVars": True, "keepTime": True}
        for target, name in DASHBOARDS if target != uid
    ]
    return layout


def table(title, query, description="", link_target=None, status_columns=()):
    result = panel(title, POSTGRES, [sql(query, "table")], "table", "short", description)
    result["options"] = {"showHeader": True, "cellHeight": "sm"}
    result["fieldConfig"]["defaults"].update(noValue="—")
    overrides = result["fieldConfig"]["overrides"]
    for name in status_columns:
        overrides.append({"matcher": {"id": "byName", "options": name}, "properties": [
            {"id": "unit", "value": "none"}, {"id": "custom.width", "value": 105},
        ]})
    if link_target:
        overrides.append({"matcher": {"id": "byName", "options": "请求 ID"},
                          "properties": [{"id": "links", "value": [{
                              "title": "查看原始正文" if link_target == "sub2api-egress-body" else "查看关联日志",
                              "url": f"/d/{link_target}?from=${{__from}}&to=${{__to}}&var-request_id=${{__value.raw:percentencode}}",
                          }]}]})
    return result


def trend(title, query, unit, description=""):
    result = panel(title, POSTGRES, [sql(query)], "timeseries", unit, description)
    result["fieldConfig"]["defaults"]["min"] = 0
    result["options"] = {"legend": {"displayMode": "list", "placement": "bottom"},
                         "tooltip": {"mode": "multi", "sort": "desc"}}
    return result


GROUP_SQL = "CASE ${group_by:sqlstring}\n" + "\n".join(
    f"WHEN '{dimension}' THEN {label}" for dimension, _, _, label in DIMENSIONS
) + "\nELSE '全部' END"
STRIDE_SQL = """CASE ${bucket:sqlstring}
WHEN '5m' THEN interval '5 minutes'
WHEN '15m' THEN interval '15 minutes'
WHEN '1h' THEN interval '1 hour'
ELSE interval '1 minute' END"""
TIME_SQL = "date_bin(step, created_at, timestamptz '2000-01-01')"
SECONDS_SQL = """greatest(1, extract(epoch FROM
  least(bucket + step, $__timeTo()::timestamptz, now()) -
  greatest(bucket, $__timeFrom()::timestamptz)))"""


def request_series(series=GROUP_SQL):
    return f"""SELECT {TIME_SQL} AS bucket, {series} AS series, step,
  outcome, app, gateway_status, provider_error, ttft_ms, tpot_ms, e2e_ms,
  input_tokens::bigint + cache_read_tokens + cache_creation_tokens AS full_input,
  output_tokens::bigint AS output_tokens, cache_read_tokens::bigint AS cached
FROM {TRAFFIC}
CROSS JOIN (SELECT {STRIDE_SQL} AS step) windows
WHERE {common_filters()}"""


def bucketed(series=GROUP_SQL):
    return f"""WITH events AS ({request_series(series)}),
agg AS (
  SELECT bucket, series, step,
    count(*) AS requests,
    sum(full_input + output_tokens) AS total_tokens,
    100.0 * sum(cached) / nullif(sum(full_input), 0) AS cache_pct,
    percentile_cont(0.50) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms > 0 AND outcome = '成功') / 1000 AS ttft_p50,
    percentile_cont(0.70) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms > 0 AND outcome = '成功') / 1000 AS ttft_p70,
    avg(ttft_ms) FILTER (WHERE ttft_ms > 0 AND outcome = '成功') / 1000 AS ttft_avg,
    percentile_cont(0.50) WITHIN GROUP (ORDER BY tpot_ms) AS tpot_p50,
    percentile_cont(0.70) WITHIN GROUP (ORDER BY tpot_ms) AS tpot_p70,
    avg(tpot_ms) AS tpot_avg,
    percentile_cont(0.50) WITHIN GROUP (ORDER BY e2e_ms) / 1000 AS e2e_p50,
    percentile_cont(0.70) WITHIN GROUP (ORDER BY e2e_ms) / 1000 AS e2e_p70,
    avg(e2e_ms) / 1000 AS e2e_avg
  FROM events GROUP BY bucket, series, step
)"""


def single_stat(expression, series=GROUP_SQL):
    return f"""{bucketed(series)}
SELECT bucket AS "time", series AS metric, {expression} AS value
FROM agg ORDER BY 1,2"""


def tpm_trend(title, series=GROUP_SQL):
    query = single_stat(f"total_tokens * 60.0 / ({SECONDS_SQL}) / {TOKENS_PER_MILLION}", series)
    return trend(title, query, f"suffix: {TPM_UNIT}", "每分钟百万 token，包含未命中输入、缓存读取和写入以及输出。")


def quantiles(prefix):
    return f"""{bucketed()}
SELECT bucket AS "time", series || ' · ' || v.stat AS metric, v.value
FROM agg
CROSS JOIN LATERAL (VALUES
  ('P50', {prefix}_p50::double precision),
  ('P70', {prefix}_p70::double precision),
  ('Avg', {prefix}_avg::double precision)
) v(stat, value)
WHERE v.value IS NOT NULL
ORDER BY 1,2"""


def error_chart(egress=False):
    if egress:
        table_name = EGRESS
        time_column = "recorded_at"
        status = "upstream_status"
        additional_filter = "outcome = '失败尝试'"
        prefix = "上游 "
        other = "coalesce(nullif(error_kind, ''), '无 HTTP 状态')"
    else:
        table_name = TRAFFIC
        time_column = "created_at"
        status = "gateway_status"
        additional_filter = "outcome = '失败'"
        prefix = "网关 "
        other = "'无 HTTP 状态'"
    return f"""WITH events AS (
  SELECT {TIME_SQL.replace('created_at', time_column)} AS bucket, step, {GROUP_SQL} AS series,
    '{prefix}' || coalesce({status}::text, {other}) ||
      coalesce(' / ' || nullif(provider_error, ''), '') AS error
  FROM {table_name} CROSS JOIN (SELECT {STRIDE_SQL} AS step) windows
  WHERE {common_filters(time_column)} AND {additional_filter}
)
SELECT bucket AS "time", series || ' · ' || error AS metric, count(*) / ({SECONDS_SQL}) AS value
FROM events GROUP BY bucket, step, series, error ORDER BY 1,2"""


def business_dashboard():
    summary = f"""WITH events AS (
  SELECT {GROUP_SQL} AS series, outcome, ttft_ms, tpot_ms, e2e_ms,
    input_tokens::bigint + cache_read_tokens + cache_creation_tokens AS full_input,
    output_tokens::bigint AS output_tokens, cache_read_tokens::bigint AS cached
  FROM {TRAFFIC} WHERE {common_filters()}
), totals AS (
  SELECT series, count(*) AS requests, count(*) FILTER (WHERE outcome='失败') AS errors,
    greatest(1, extract(epoch FROM least($__timeTo()::timestamptz, now()) - $__timeFrom()::timestamptz)) AS seconds,
    count(*) FILTER (WHERE ttft_ms > 0 AND outcome='成功') AS ttft_samples,
    percentile_cont(.50) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms > 0 AND outcome='成功') / 1000 AS ttft_p50,
    percentile_cont(.70) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms > 0 AND outcome='成功') / 1000 AS ttft_p70,
    avg(ttft_ms) FILTER (WHERE ttft_ms > 0 AND outcome='成功') / 1000 AS ttft_avg,
    count(tpot_ms) AS tpot_samples,
    percentile_cont(.50) WITHIN GROUP (ORDER BY tpot_ms) AS tpot_p50,
    percentile_cont(.70) WITHIN GROUP (ORDER BY tpot_ms) AS tpot_p70,
    avg(tpot_ms) AS tpot_avg,
    count(e2e_ms) AS e2e_samples,
    percentile_cont(.50) WITHIN GROUP (ORDER BY e2e_ms) / 1000 AS e2e_p50,
    percentile_cont(.70) WITHIN GROUP (ORDER BY e2e_ms) / 1000 AS e2e_p70,
    avg(e2e_ms) / 1000 AS e2e_avg,
    sum(full_input + output_tokens) AS tokens, 100.0 * sum(cached) / nullif(sum(full_input), 0) AS cache_pct
  FROM events GROUP BY series
)
SELECT series AS "聚合项", requests AS "请求数", round(requests / seconds, 2) AS "QPS",
  errors AS "错误数", round(errors / seconds, 2) AS "错误 QPS",
  round(tokens * 60.0 / seconds / {TOKENS_PER_MILLION}, {METRIC_DECIMALS}) AS "{TPM_UNIT}", round(cache_pct, 2) AS "缓存命中率 %",
  round(ttft_p50::numeric, 2) AS "TTFT P50 s", round(ttft_p70::numeric, 2) AS "TTFT P70 s", round(ttft_avg, 2) AS "TTFT Avg s", ttft_samples AS "TTFT 样本",
  round(tpot_p50::numeric, 2) AS "TPOT P50 ms", round(tpot_p70::numeric, 2) AS "TPOT P70 ms", round(tpot_avg::numeric, 2) AS "TPOT Avg ms", tpot_samples AS "TPOT 样本",
  round(e2e_p50::numeric, 2) AS "端到端 P50 s", round(e2e_p70::numeric, 2) AS "端到端 P70 s", round(e2e_avg, 2) AS "端到端 Avg s", e2e_samples AS "端到端样本"
FROM totals ORDER BY requests DESC, series LIMIT 500"""
    apps = f"""WITH counts AS (
  SELECT {GROUP_SQL} AS series, app, count(*) AS requests
  FROM {TRAFFIC} WHERE {common_filters()} AND request_kind = 'model' GROUP BY 1, 2
)
SELECT series AS "聚合项", app AS "应用", requests AS "模型请求数",
  round(100.0 * requests / nullif(sum(requests) OVER (PARTITION BY series), 0), 2) AS "占比 %"
FROM counts ORDER BY series, requests DESC, app"""
    auxiliary = f"""WITH counts AS (
  SELECT {GROUP_SQL} AS series, request_kind, count(*) AS requests,
    count(*) FILTER (WHERE outcome = '失败') AS errors
  FROM {TRAFFIC} WHERE {common_filters()} AND request_kind IN ('count_tokens', 'models') GROUP BY 1, 2
)
SELECT series AS "聚合项",
  CASE request_kind WHEN 'count_tokens' THEN 'Token 计数' WHEN 'models' THEN '模型列表' END AS "调用类型",
  requests AS "请求数", errors AS "错误数"
FROM counts ORDER BY series, request_kind"""
    errors = f"""WITH errors AS (
  SELECT {GROUP_SQL} AS series, gateway_status, upstream_status, provider_error, error_type,
    count(*) AS errors
  FROM {TRAFFIC} WHERE {common_filters()} AND outcome = '失败' GROUP BY 1, 2, 3, 4, 5
)
SELECT series AS "聚合项", gateway_status AS "网关错误码", upstream_status AS "上游记录码",
  provider_error AS "厂商错误码", error_type AS "错误类型", errors AS "次数",
  round(errors / greatest(1, extract(epoch FROM least($__timeTo()::timestamptz, now()) - $__timeFrom()::timestamptz)), 2) AS "错误 QPS"
FROM errors ORDER BY errors DESC LIMIT 500"""
    summary_panel = table("选定时间范围汇总", summary, "QPS 以网关完成记录为准，匹配的访问日志不重复计数。重试尝试单列。空值表示该口径缺少样本。")
    summary_panel["fieldConfig"]["overrides"].append({
        "matcher": {"id": "byName", "options": TPM_UNIT},
        "properties": [{"id": "unit", "value": "none"}, {"id": "decimals", "value": METRIC_DECIMALS}],
    })
    qps_panel = trend("QPS", single_stat(f"requests / ({SECONDS_SQL})"), "suffix: req/s")
    apps_panel = table("应用分布", apps, "以模型请求为分母。Claude Desktop 与 Claude Code 合并统计。")
    apps_panel["fieldConfig"]["overrides"].append({
        "matcher": {"id": "byName", "options": "占比 %"},
        "properties": [{"id": "unit", "value": "none"}, {"id": "decimals", "value": METRIC_DECIMALS}],
    })
    auxiliary_panel = table("辅助调用", auxiliary, "Token 计数和模型列表的请求数及错误数。历史访问日志可能缺少应用和 API Key。")
    result = layout_dashboard("sub2api-traffic", "流量指标", [
        [summary_panel],
        [qps_panel, apps_panel],
        [trend("TTFT", quantiles("ttft"), "suffix: s", "成功请求的 Sub2API 首事件耗时。"),
         trend("TPOT（估算）", quantiles("tpot"), "suffix: ms", "流式成功请求（转发总耗时 − TTFT）÷（输出 token − 1）；上游计数可能含推理 token。")],
        [trend("端到端延迟", quantiles("e2e"), "suffix: s", "从网关收到请求到处理结束，按已关联的访问日志。覆盖情况见汇总的样本数。"),
         tpm_trend("TPM")],
        [trend("输入缓存命中率", single_stat("cache_pct"), "percent", "缓存读取 token ÷（未命中输入 + 缓存读取 + 缓存写入），按 token 加权。"),
         trend("错误 QPS · 按网关错误码", error_chart(), "suffix: req/s", "按最终网关错误码和厂商错误码拆分，包含已记录的业务限制。")],
        [trend("上游失败尝试 QPS · 按错误码", error_chart(egress=True), "suffix: attempt/s", "尝试级统计，包括重试后恢复的错误。"),
         table("网关与上游错误码明细", errors, "高频错误按状态码和类型展开。", status_columns=("网关错误码", "上游记录码", "厂商错误码"))],
        [tpm_trend("API Key TPM", "key_label"),
         tpm_trend("上游账号 TPM", "account_label"),
         tpm_trend("分组 TPM", "group_label")],
    ], [
        choice("group_by", "聚合维度", [("all", "全部")] + [(k, label) for k, label, _, _ in DIMENSIONS], "all"),
        choice("bucket", "时间粒度", [("1m", "1 分钟"), ("5m", "5 分钟"), ("15m", "15 分钟"), ("1h", "1 小时")], "1m"),
        *dimension_variables(),
    ])
    qps_panel["gridPos"]["w"] = 8
    apps_panel["gridPos"].update(x=8, w=8)
    auxiliary_panel.update(
        id=max(item["id"] for item in result["panels"]) + 1,
        gridPos={**apps_panel["gridPos"], "x": 16},
    )
    result["panels"].append(auxiliary_panel)
    result["refresh"] = "30s"
    return result


REQUEST_FILTERED = f"""FROM {REQUESTS}
WHERE {common_filters(request_id='full')} AND {status_filter()} AND {path_filter()}"""
EGRESS_FILTERED = f"""FROM {EGRESS}
WHERE {common_filters('recorded_at', request_id='full')} AND {status_filter()} AND {path_filter('upstream_endpoint')}"""
ACCESS_FILTERED = f"""FROM {ACCESS}
WHERE {common_filters(request_id='full')} AND {status_filter()} AND {path_filter()}"""


def log_variables():
    return [*dimension_variables(), textbox("request_id", "请求 ID"),
            textbox("status", "错误码 / HTTP 状态"), textbox("path", "路径")]


def logs_dashboard():
    global_rows = f"""WITH
requests AS (
  SELECT created_at, CASE WHEN outcome='成功' THEN '成功请求' ELSE '网关失败' END AS kind,
    request_id, account_label, group_label, key_label, app, model, endpoint,
    coalesce(gateway_status::text, '') AS status, coalesce(error_message, '') AS detail
  {REQUEST_FILTERED}
  ORDER BY created_at DESC LIMIT 500
),
egress AS (
  SELECT event_at, '上游失败尝试', request_id, account_label, group_label, key_label, app, model,
    upstream_endpoint, coalesce(upstream_status::text, provider_error, ''),
    coalesce(result_summary, error_kind, '')
  {EGRESS_FILTERED} AND outcome = '失败尝试'
  ORDER BY recorded_at DESC LIMIT 500
),
access AS (
  SELECT created_at, 'HTTP 访问', request_id, account_label, group_label, key_label, app, model,
    endpoint, gateway_status::text, method
  {ACCESS_FILTERED}
  ORDER BY created_at DESC LIMIT 500
),
diagnostic AS (
  SELECT created_at, '应用诊断', request_id, account_label, group_label, key_label, app, model,
    endpoint, level, component || ' · ' || message
  FROM {DIAGNOSTICS}
  WHERE {common_filters(request_id=True)} AND ${{status:sqlstring}} = '' AND {path_filter()}
  ORDER BY created_at DESC LIMIT 500
),
merged AS (
  SELECT * FROM requests UNION ALL SELECT * FROM egress
  UNION ALL SELECT * FROM access UNION ALL SELECT * FROM diagnostic
)
SELECT created_at AS "time", kind AS "日志类型", request_id AS "请求 ID", app AS "应用", model AS "模型",
  account_label AS "上游账号", group_label AS "分组", key_label AS "API Key", endpoint AS "路径",
  status AS "状态 / 级别", detail AS "明细"
FROM merged
WHERE ${{log_type:sqlstring}} = '全部' OR kind = ${{log_type:sqlstring}}
ORDER BY created_at DESC LIMIT 500"""
    access = f"""SELECT created_at AS "time", request_id AS "请求 ID", method AS "方法", endpoint AS "路径",
  gateway_status AS "HTTP 状态", round(e2e_ms / 1000.0, 2) AS "端到端 s", model AS "模型", app AS "应用",
  account_label AS "上游账号", group_label AS "分组", key_label AS "API Key", client_id AS "客户端请求 ID",
  timing_source AS "来源"
{ACCESS_FILTERED}
ORDER BY created_at DESC LIMIT 500"""
    success = f"""SELECT created_at AS "time", request_id AS "请求 ID", app AS "应用",
  requested_model AS "请求模型", upstream_model AS "发送模型", response_model AS "响应模型",
  account_label AS "上游账号", group_label AS "分组", key_label AS "API Key", endpoint AS "入站路径",
  wire_status AS "HTTP 状态", round(ttft_ms / 1000.0, 2) AS "TTFT s", round(tpot_ms::numeric, 2) AS "TPOT ms",
  round(e2e_ms / 1000.0, 2) AS "端到端 s", round(forward_ms / 1000.0, 2) AS "转发耗时 s",
  input_tokens AS "未命中输入 token", cache_read_tokens AS "缓存读 token", cache_creation_tokens AS "缓存写 token",
  output_tokens AS "输出 token", reasoning_effort AS "推理档位", upstream_id AS "上游请求 ID",
  gateway_id AS "网关请求 ID", timing_source AS "端到端数据源"
{REQUEST_FILTERED} AND outcome='成功'
ORDER BY created_at DESC LIMIT 500"""
    errors = f"""SELECT created_at AS "time", request_id AS "请求 ID", app AS "应用", model AS "模型", endpoint AS "入站路径",
  account_label AS "上游账号", group_label AS "分组", key_label AS "API Key",
  gateway_status AS "网关错误码", wire_status AS "实际 HTTP 状态", upstream_status AS "上游记录码", provider_error AS "厂商错误码",
  error_type AS "错误类型", phase AS "阶段", network_error AS "网络错误", business_limited AS "业务限制",
  round(e2e_ms / 1000.0, 2) AS "端到端 s", error_message AS "错误消息", client_id AS "客户端请求 ID"
{REQUEST_FILTERED} AND outcome='失败'
ORDER BY created_at DESC LIMIT 500"""
    diag = f"""SELECT created_at AS "time", request_id AS "请求 ID", level AS "级别", component AS "组件",
  message AS "消息", model AS "模型", app AS "应用", account_label AS "上游账号", group_label AS "分组", key_label AS "API Key", endpoint AS "路径"
FROM {DIAGNOSTICS}
WHERE {common_filters(request_id=True)} AND ${{status:sqlstring}} = '' AND {path_filter()}
ORDER BY created_at DESC LIMIT 500"""
    return layout_dashboard("sub2api-logs", "全局日志", [
        [table("全局日志明细", global_rows, "同一请求可有多种事件。点击请求 ID 查看出口。", "sub2api-egress")],
        [table("HTTP 访问明细", access, "访问日志按请求 ID 关联模型、账号、分组、Key 和应用。", "sub2api-egress", ("HTTP 状态",))],
        [table("成功请求明细", success, "一行对应一条成功用量记录。端到端与转发耗时分别展示。", "sub2api-egress", ("HTTP 状态",))],
        [table("网关错误明细", errors, "网关逻辑错误码与 HTTP 状态分开显示，便于排查已经返回 200 的流中错误。", "sub2api-egress",
               ("网关错误码", "实际 HTTP 状态", "上游记录码", "厂商错误码"))],
        [table("应用诊断明细", diag, "系统级事件若缺少关联上下文，保留在未归属项。", "sub2api-egress")],
    ], [*log_variables(), choice("log_type", "全局表类型",
            [(v, v) for v in ("全部", "HTTP 访问", "成功请求", "网关失败", "上游失败尝试", "应用诊断")], "全部")], hours=1)


def egress_dashboard():
    request_detail = f"""SELECT event_at AS "time", outcome AS "结果", request_id AS "请求 ID",
  app AS "应用", account_label AS "上游账号", group_label AS "分组", key_label AS "API Key",
  requested_model AS "请求模型", upstream_model AS "发送模型", response_model AS "响应模型",
  upstream_endpoint AS "上游路由", upstream_url AS "实际 URL（已记录）", proxy AS "代理",
  attempt AS "尝试序号", upstream_status AS "上游状态码", gateway_status AS "网关状态码",
  provider_error AS "厂商错误码", error_kind AS "错误类型", error_stage AS "阶段",
  result_summary AS "结果摘要", upstream_id AS "上游请求 ID", source AS "来源"
{EGRESS_FILTERED}
  AND (${{outcome:sqlstring}} = '全部' OR outcome = ${{outcome:sqlstring}})
ORDER BY recorded_at DESC, event_at DESC LIMIT 500"""
    result_detail = f"""SELECT event_at AS "time", request_id AS "请求 ID", outcome AS "结果",
  account_label AS "上游账号", app AS "应用", upstream_model AS "发送模型", response_model AS "响应模型",
  round(ttft_ms / 1000.0, 2) AS "TTFT s", headers_ms AS "上游响应头 ms",
  round(forward_ms / 1000.0, 2) AS "转发耗时 s", input_tokens AS "未命中输入 token", output_tokens AS "输出 token",
  cache_read_tokens AS "缓存读取 token", cache_creation_tokens AS "缓存写入 token",
  reasoning_effort AS "推理档位", service_tier AS "服务档位", gateway_id AS "网关请求 ID",
  upstream_id AS "上游请求 ID", source AS "来源"
{EGRESS_FILTERED}
  AND (${{outcome:sqlstring}} = '全部' OR outcome = ${{outcome:sqlstring}})
ORDER BY recorded_at DESC, event_at DESC LIMIT 500"""
    return layout_dashboard("sub2api-egress", "上游出口请求与结果", [
        [table("出口请求及结果明细", request_detail,
               "成功行来自最终用量记录，失败行来自上游尝试事件；点击请求 ID 查看测试 Key 的原始正文和请求头。URL 和上游状态依记录显示。",
               "sub2api-egress-body", ("上游状态码", "网关状态码", "厂商错误码"))],
        [table("出口耗时、用量与关联 ID", result_detail,
               "响应头耗时、上游请求 ID 等字段只在来源已记录时显示。", "sub2api-egress-body")],
    ], [*log_variables(), choice("outcome", "结果", [(v, v) for v in ("全部", "成功", "失败尝试")], "全部")], hours=1)



def egress_body_dashboard():
    query = """FROM grafana_sub2api.egress_payload_details
WHERE ${request_id:sqlstring} <> ''
  AND (gateway_id = ${request_id:sqlstring} OR client_id = ${request_id:sqlstring}
       OR 'local:' || gateway_id = ${request_id:sqlstring}
       OR 'client:' || client_id = ${request_id:sqlstring})"""
    metadata = f"""SELECT created_at AS "time", id AS "采集序号",
  gateway_id AS "网关请求 ID", client_id AS "客户端请求 ID", key_label AS "测试 Key",
  account_label AS "上游账号", method AS "协议 / 方法", url AS "上游 URL", status_code AS "状态",
  request_json->>'complete' AS "请求完整", response_json->>'complete' AS "响应完整", failure AS "传输异常"
{query}
ORDER BY created_at DESC, id DESC LIMIT 16"""
    def body_panel(title, field):
        statement = f"""SELECT created_at AS "time", jsonb_pretty({field}) AS line,
  id::text AS "采集序号", method AS "协议 / 方法"
{query}
ORDER BY created_at ASC, id ASC LIMIT 16"""
        return panel(title, POSTGRES, [sql(statement, "table")], "sub2api-jsoncopy-panel")
    result = layout_dashboard("sub2api-egress-body", "上游正文详情", [
        [table("已采集的出口尝试", metadata,
               "从出口明细点请求 ID 进入。仅展示测试 Key 已采集且在 24 小时保留期内的请求。",
               status_columns=("状态",))],
        [body_panel("发给上游的请求 JSON", "request_json")],
        [body_panel("上游响应 JSON / 流式事件", "response_json")],
    ], [textbox("request_id", "请求 ID")], hours=24)
    for item in result["panels"][1:]:
        item["gridPos"]["h"] = 17
    result["panels"][-1]["gridPos"]["y"] = result["panels"][1]["gridPos"]["y"] + 17
    return result

def metric(namespace, name, dims, statistic="Average", period="300", label=None):
    query = {"queryMode": "Metrics", "metricQueryType": 0, "metricEditorMode": 0,
             "region": REGION, "namespace": namespace, "metricName": name,
             "dimensions": dims, "statistic": statistic, "period": period, "matchExact": True}
    if label:
        query["label"] = label
    return query


def aws_dashboard(args):
    alb = {"LoadBalancer": args.alb}
    rds = {"DBInstanceIdentifier": "sub2api-sg-pg"}
    cache = {"CacheClusterId": "sub2api-sg-kv-001", "CacheNodeId": "0001"}
    app = {"InstanceId": args.app}
    grafana = {"InstanceId": args.grafana}
    return dashboard("sub2api-aws", "Sub2API AWS 基础设施", [
        panel("应用机与 Grafana CPU", CLOUDWATCH, [metric("AWS/EC2", "CPUUtilization", app, label="Sub2API"), metric("AWS/EC2", "CPUUtilization", grafana, label="Grafana")], unit="percent"),
        panel("应用机与 Grafana CPU 积分", CLOUDWATCH, [metric("AWS/EC2", "CPUCreditBalance", app, "Minimum", label="Sub2API"), metric("AWS/EC2", "CPUCreditBalance", grafana, "Minimum", label="Grafana")]),
        panel("ALB 请求量", CLOUDWATCH, [metric("AWS/ApplicationELB", "RequestCount", alb, "Sum")]),
        panel("ALB 5xx", CLOUDWATCH, [metric("AWS/ApplicationELB", "HTTPCode_ELB_5XX_Count", alb, "Sum", label="ALB"), metric("AWS/ApplicationELB", "HTTPCode_Target_5XX_Count", alb, "Sum", label="Target")], description="AWS 可能在没有 5xx 时省略数据点。"),
        panel("ALB 到应用的响应头耗时 P95", CLOUDWATCH, [metric("AWS/ApplicationELB", "TargetResponseTime", alb, "p95")], unit="s"),
        panel("ALB 健康目标数", CLOUDWATCH, [metric("AWS/ApplicationELB", "HealthyHostCount", dict(alb, TargetGroup=args.target), "Minimum")]),
        panel("RDS CPU", CLOUDWATCH, [metric("AWS/RDS", "CPUUtilization", rds)], unit="percent"),
        panel("RDS 连接数", CLOUDWATCH, [metric("AWS/RDS", "DatabaseConnections", rds)]),
        panel("Valkey 引擎 CPU", CLOUDWATCH, [metric("AWS/ElastiCache", "EngineCPUUtilization", cache)], unit="percent"),
        panel("Valkey 内存占用", CLOUDWATCH, [metric("AWS/ElastiCache", "DatabaseMemoryUsagePercentage", cache)], unit="percent"),
    ], split=True)


def main():
    parser = argparse.ArgumentParser()
    for name in ("account", "app", "grafana", "alb", "target"):
        parser.add_argument(f"--{name}", required=True)
    parser.add_argument("destination", type=Path)
    args = parser.parse_args()
    args.destination.mkdir(parents=True, exist_ok=True)
    for item in [logs_dashboard(), business_dashboard(), egress_dashboard(), egress_body_dashboard(), aws_dashboard(args)]:
        (args.destination / f"{item['uid']}.json").write_text(json.dumps(item, ensure_ascii=False), encoding="utf-8")


if __name__ == "__main__":
    main()
