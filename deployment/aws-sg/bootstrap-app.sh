#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ $# -ne 4 ]]; then
  echo 'Usage: bootstrap-app.sh REGION STACK DATABASE_HOST REDIS_HOST' >&2
  exit 1
fi

export AWS_DEFAULT_REGION=$1
export AWS_PAGER=""
stack=$2
database_host=$3
redis_host=$4
cd /opt/sub2api

compose() {
  docker compose --env-file .env.shared --env-file .env.node \
    -f compose.standalone.yaml -f compose.aws.yaml -f compose.cloudwatch.yaml "$@"
}

check_setup() {
  curl -fsS --max-time 10 http://127.0.0.1:18080/health | jq -e '.status == "ok"' >/dev/null &&
    curl -fsS --max-time 10 http://127.0.0.1:18080/setup/status |
      jq -e '.data.needs_setup == false' >/dev/null
}

if [[ -f .env.node ]] && grep -qx 'SKIP_SETUP=true' .env.node && check_setup; then
  echo 'Sub2API app-01 is already initialized and healthy.'
  exit 0
fi

secret() {
  aws secretsmanager get-secret-value --secret-id "$stack/$1" --query SecretString --output text
}

db_credentials=$(secret app-db)
database_password=$(jq -er '.password' <<<"$db_credentials")
redis_password=$(secret valkey | jq -er '.password')
jwt_secret=$(secret jwt)
totp_key=$(secret totp)
admin_credentials=$(secret bootstrap-admin)
admin_email=$(jq -er '.email' <<<"$admin_credentials")
admin_password=$(jq -er '.password' <<<"$admin_credentials")

[[ "$database_password" =~ ^[a-zA-Z0-9]{48}$ && "$redis_password" =~ ^[a-zA-Z0-9]{48}$ ]]
[[ "$jwt_secret" =~ ^[0-9a-f]{64}$ && "$totp_key" =~ ^[0-9a-f]{64}$ && "$jwt_secret" != "$totp_key" ]]
[[ "$admin_password" =~ ^[a-zA-Z0-9]{48}$ && "$admin_email" == admin@sub2api.invalid ]]

{
  sed -E '/^(DATABASE_HOST|DATABASE_PASSWORD|REDIS_HOST|REDIS_PASSWORD|JWT_SECRET|TOTP_ENCRYPTION_KEY)=$/d' .env.shared.example
  printf '\nDATABASE_HOST=%s\nDATABASE_PASSWORD=%s\n' "$database_host" "$database_password"
  printf 'REDIS_HOST=%s\nREDIS_PASSWORD=%s\n' "$redis_host" "$redis_password"
  printf 'JWT_SECRET=%s\nTOTP_ENCRYPTION_KEY=%s\n' "$jwt_secret" "$totp_key"
  printf 'DEPLOY_REGION=%s\nAPP_LOG_GROUP=/sub2api/%s/app\n' "$AWS_DEFAULT_REGION" "$stack"
} >.env.shared.next
mv .env.shared.next .env.shared

{
  printf 'BIND_HOST=10.10.10.11\nAPP_NODE_NAME=app-01\nAUTO_SETUP=true\nSKIP_SETUP=false\n'
  printf 'ADMIN_EMAIL=%s\nADMIN_PASSWORD=%s\n' "$admin_email" "$admin_password"
} >.env.node.next
mv .env.node.next .env.node

compose config --quiet
compose pull sub2api
compose up -d --wait --wait-timeout 600 sub2api
check_setup

printf '%s\n' "$admin_credentials" |
  curl -fsS --max-time 10 --json @- http://127.0.0.1:18080/api/v1/auth/login |
  jq -e '.code == 0 and (.data.access_token | length > 0)' >/dev/null

printf 'BIND_HOST=10.10.10.11\nAPP_NODE_NAME=app-01\nAUTO_SETUP=false\nSKIP_SETUP=true\n' >.env.node.next
mv .env.node.next .env.node
compose up -d --wait --wait-timeout 600 sub2api
check_setup

echo 'Sub2API app-01 is healthy. Database migrations, setup status, and admin login passed.'
