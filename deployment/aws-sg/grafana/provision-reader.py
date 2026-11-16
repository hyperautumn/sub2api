import base64
import hashlib
import hmac
import json
from pathlib import Path
import secrets
import subprocess
import tempfile

import psycopg
from psycopg import sql


AWS = ["aws", "--profile", "sg-power", "--region", "ap-southeast-1", "--no-cli-pager"]


def aws(*args):
    return subprocess.check_output(AWS + list(args), text=True).strip()


def scram_verifier(password):
    salt = secrets.token_bytes(16)
    iterations = 4096
    salted = hashlib.pbkdf2_hmac("sha256", password.encode(), salt, iterations)
    stored_key = hashlib.sha256(hmac.digest(salted, b"Client Key", "sha256")).digest()
    server_key = hmac.digest(salted, b"Server Key", "sha256")
    salt64, stored64, server64 = (base64.b64encode(item).decode() for item in (salt, stored_key, server_key))
    return f"SCRAM-SHA-256${iterations}:{salt64}${stored64}:{server64}"


def main():
    rds = json.loads(aws("rds", "describe-db-instances", "--db-instance-identifier", "sub2api-sg-pg",
                         "--query", "DBInstances[0].{host:Endpoint.Address,secret:MasterUserSecret.SecretArn}"))
    master = json.loads(aws("secretsmanager", "get-secret-value", "--secret-id", rds["secret"],
                            "--query", "SecretString", "--output", "text"))
    password = aws("ssm", "get-parameter", "--name", "/sub2api/grafana/reader-password", "--with-decryption",
                   "--query", "Parameter.Value", "--output", "text")
    with tempfile.TemporaryDirectory(prefix="grafana-rds-") as temporary:
        ca = Path(temporary) / "rds-ca.pem"
        subprocess.run(["curl", "-fsSL", "--max-time", "15", "-o", str(ca),
                        "https://truststore.pki.rds.amazonaws.com/ap-southeast-1/ap-southeast-1-bundle.pem"], check=True)
        db = dict(host=rds["host"], hostaddr="127.0.0.1", port=15432, dbname="sub2api", sslmode="verify-full",
                  sslrootcert=str(ca), connect_timeout=12)
        with psycopg.connect(**db, user=master["username"], password=master["password"], autocommit=True) as connection:
            source = Path(__file__).with_name("monitoring.sql").read_text()
            verifier = sql.Literal(scram_verifier(password)).as_string(connection)
            connection.execute(source.replace("__GRAFANA_READER_VERIFIER__", verifier))
            connection.execute("SET ROLE sub2api")
            connection.execute(Path(__file__).with_name("observability.sql").read_text())
            connection.execute(Path(__file__).with_name("egress-payloads.sql").read_text())

        with psycopg.connect(**db, user="grafana_sub2api_reader", password=password) as connection:
            readonly, api_keys, accounts, requests, errors = connection.execute("""
                SELECT current_setting('transaction_read_only'),
                       has_table_privilege('public.api_keys', 'SELECT'),
                       has_table_privilege('public.accounts', 'SELECT'),
                       (SELECT count(*) FROM grafana_sub2api.requests WHERE created_at > now() - interval '24 hours'),
                       (SELECT count(*) FROM grafana_sub2api.errors WHERE created_at > now() - interval '24 hours')
            """).fetchone()
            if readonly != "on" or api_keys or accounts:
                raise SystemExit("Unexpected Grafana reader permissions")
            print(f"Verified read-only views. Requests in 24h: {requests}. Errors in 24h: {errors}.")


if __name__ == "__main__":
    main()
