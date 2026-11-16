"""Copy only dashboard timing and correlation fields from retained access logs."""

import argparse
import base64
import csv
from datetime import datetime, timedelta, timezone
import gzip
import io
import json
import re
import subprocess

AWS = ["aws", "--profile", "sg-power", "--region", "ap-southeast-1", "--no-cli-pager"]
LOG_GROUP = "/sub2api/sub2api-sg/app"
API_PATH = re.compile(r"^/(?:v1(?:beta)?/|anthropic/|openai/|gemini/)[^?\s#]{1,240}$")
REQUEST_ID = re.compile(r"^[A-Za-z0-9._:/-]{1,128}$")


def aws(*args):
    return subprocess.check_output(AWS + list(args), text=True).strip()


def request_id(value):
    return value if isinstance(value, str) and REQUEST_ID.fullmatch(value) else ""


def access_rows(days):
    end = datetime.now(timezone.utc)
    start = end - timedelta(days=days)
    events = json.loads(aws(
        "logs", "filter-log-events", "--log-group-name", LOG_GROUP,
        "--start-time", str(int(start.timestamp() * 1000)), "--end-time", str(int(end.timestamp() * 1000)),
        "--filter-pattern", '"http request completed"', "--query", "events", "--output", "json",
    ))
    for event in events:
        try:
            fields = event["message"].rstrip().split("\t", 4)
            meta = json.loads(fields[4])
            endpoint, method = meta.get("path", ""), meta.get("method", "")
            if fields[3] != "http request completed" or not API_PATH.fullmatch(endpoint) or not re.fullmatch("[A-Z]{3,12}", method):
                continue
            status, latency = meta["status_code"], meta["latency_ms"]
            gateway_id, client_id = request_id(meta.get("request_id")), request_id(meta.get("client_request_id"))
            if not (gateway_id or client_id) or not 100 <= status <= 599 or not 0 <= latency < 10**14:
                continue
            timestamp = datetime.fromtimestamp(event["timestamp"] / 1000, timezone.utc).isoformat()
            yield event["eventId"], timestamp, gateway_id, client_id, method, endpoint, status, latency
        except (ValueError, TypeError, IndexError, KeyError):
            continue


def import_rows(rows, instance):
    data = io.StringIO()
    csv.writer(data, lineterminator="\n").writerows(rows)
    sql = f"""BEGIN;
CREATE TEMP TABLE buffer (LIKE grafana_sub2api.gateway_access_history);
COPY buffer FROM STDIN (FORMAT csv);
{data.getvalue()}\\.
WITH inserted AS (
  INSERT INTO grafana_sub2api.gateway_access_history SELECT * FROM buffer ON CONFLICT DO NOTHING RETURNING 1
)
SELECT count(*) FROM inserted;
COMMIT;
"""
    encoded = base64.b64encode(gzip.compress(sql.encode())).decode()
    shell = f"""set -eu
printf '%s' '{encoded}' | base64 -d | gzip -dc |
  docker exec -i sub2api sh -lc 'export PGPASSWORD="$DATABASE_PASSWORD" PGSSLMODE="$DATABASE_SSLMODE"; exec psql -X -q -A -t -v ON_ERROR_STOP=1 -h "$DATABASE_HOST" -U "$DATABASE_USER" -d "$DATABASE_DBNAME"'
"""
    command = aws(
        "ssm", "send-command", "--instance-ids", instance, "--document-name", "AWS-RunShellScript",
        "--timeout-seconds", "45", "--comment", "Backfill sanitized Sub2API gateway access timing",
        "--parameters", json.dumps({"commands": [shell]}), "--query", "Command.CommandId", "--output", "text",
    )
    aws("ssm", "wait", "command-executed", "--instance-id", instance, "--command-id", command)
    return int(aws("ssm", "get-command-invocation", "--instance-id", instance, "--command-id", command,
                   "--query", "StandardOutputContent", "--output", "text"))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("instance", help="Managed Sub2API EC2 instance")
    parser.add_argument("--days", type=int, choices=range(1, 31), default=30)
    args = parser.parse_args()
    rows = list(access_rows(args.days))
    inserted = sum(import_rows(rows[i:i+150], args.instance) for i in range(0, len(rows), 150))
    print(f"API access records found: {len(rows)}; imported: {inserted}.")


if __name__ == "__main__":
    main()
