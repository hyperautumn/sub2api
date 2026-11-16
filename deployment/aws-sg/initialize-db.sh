#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo 'Usage: initialize-db.sh REGION STACK DATABASE_HOST MASTER_SECRET_ARN' >&2
  exit 1
fi

export AWS_DEFAULT_REGION=$1
export AWS_PAGER=""
stack=$2
database_host=$3
master_secret=$4
cd /opt/sub2api
image=$(sed -n 's/^SUB2API_IMAGE=//p' .env.shared.example)

read_secret() {
  aws secretsmanager get-secret-value --secret-id "$1" --query SecretString --output text
}

admin=$(read_secret "$master_secret")
app=$(read_secret "$stack/app-db")
master_user=$(jq -er '.username' <<<"$admin")
master_password=$(jq -er '.password' <<<"$admin")
app_user=$(jq -er '.username' <<<"$app")
app_password=$(jq -er '.password' <<<"$app")
[[ "$master_user" == sub2api_admin && "$app_user" == sub2api && "$app_password" =~ ^[a-zA-Z0-9]{48}$ ]]

# Send a PostgreSQL SCRAM verifier in the DDL so no plaintext password is
# passed as an argument or recorded in the SQL statement log.
verifier=$(printf '%s' "$app_password" | python3 -c '
import base64
import hashlib
import hmac
import secrets
import sys

salt = secrets.token_bytes(16)
iterations = 4096
salted = hashlib.pbkdf2_hmac("sha256", sys.stdin.buffer.read(), salt, iterations)
stored_key = hashlib.sha256(hmac.digest(salted, b"Client Key", "sha256")).digest()
server_key = hmac.digest(salted, b"Server Key", "sha256")
salt, stored_key, server_key = [base64.b64encode(x).decode() for x in (salt, stored_key, server_key)]
print(f"SCRAM-SHA-256${iterations}:{salt}${stored_key}:{server_key}")
')

psql() {
  docker run --rm -i --network host --env PGPASSWORD --env PGSSLMODE=verify-full \
    --env PGSSLROOTCERT=/rds-ca.pem --env PGCONNECT_TIMEOUT=12 \
    --mount "type=bind,source=$PWD/rds-ap-southeast-1.pem,target=/rds-ca.pem,readonly" \
    --entrypoint /usr/local/bin/psql "$image" \
    -X -q -v ON_ERROR_STOP=1 --host "$database_host" --dbname sub2api "$@"
}

PGPASSWORD="$master_password" psql --username "$master_user" <<SQL
BEGIN;
SELECT 'CREATE ROLE sub2api LOGIN PASSWORD ''$verifier'''
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sub2api')
\gexec
GRANT sub2api TO sub2api_admin WITH SET TRUE;
ALTER DATABASE sub2api OWNER TO sub2api;
COMMIT;
SQL

owner=$(PGPASSWORD="$app_password" psql --username "$app_user" --no-align --tuples-only <<'SQL'
CREATE EXTENSION IF NOT EXISTS pg_trgm;
SELECT pg_get_userbyid(datdba) = current_user
FROM pg_database
WHERE datname = current_database();
SQL
)
[[ "$owner" == t ]]
echo 'Verified RDS TLS, least-privilege application login, database ownership, and pg_trgm.'
