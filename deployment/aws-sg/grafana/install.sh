#!/usr/bin/env bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive AWS_DEFAULT_REGION=ap-southeast-1 AWS_PAGER=''

# The host uses a private subnet with HTTPS-only internet egress.
sed -i -E 's@https?://[^ /]+\.clouds\.ports\.ubuntu\.com@https://ports.ubuntu.com@g; s@http://([^ ]*ubuntu\.com)@https://\1@g' /etc/apt/sources.list.d/ubuntu.sources
apt_opts=(-o DPkg::Lock::Timeout=120 -o Acquire::Retries=3 -o Acquire::https::Timeout=20)
install -d -m 0755 /etc/apt/keyrings
curl -fsSL --retry 3 https://apt.grafana.com/gpg-full.key -o /etc/apt/keyrings/grafana.asc
printf '%s\n' 'deb [signed-by=/etc/apt/keyrings/grafana.asc] https://apt.grafana.com stable main' > /etc/apt/sources.list.d/grafana.list
apt-get "${apt_opts[@]}" update -qq
apt-get "${apt_opts[@]}" install -y -qq grafana
json_panel="$(dirname -- "${BASH_SOURCE[0]}")/sub2api-jsoncopy-panel"
install -d -m 0755 /var/lib/grafana/plugins/sub2api-jsoncopy-panel
install -m 0644 "$json_panel/plugin.json" "$json_panel/module.js" /var/lib/grafana/plugins/sub2api-jsoncopy-panel/

if ! command -v aws >/dev/null; then
  installer=$(mktemp -d)
  trap 'rm -rf "$installer"' EXIT
  curl -fsSL --retry 3 https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip -o "$installer/aws.zip"
  python3 -m zipfile -e "$installer/aws.zip" "$installer"
  chmod +x "$installer/aws/install" "$installer/aws/dist/aws"
  "$installer/aws/install" --update
fi

install -d -m 0750 -o root -g grafana /etc/grafana/credentials
install -m 0640 -o root -g grafana /dev/null /etc/grafana/credentials/admin-password
aws ssm get-parameter --name /sub2api/grafana/admin-password --with-decryption \
  --query Parameter.Value --output text > /etc/grafana/credentials/admin-password

cat > /etc/grafana/grafana.ini <<'INI'
[server]
http_addr = 127.0.0.1
http_port = 3000
root_url = http://127.0.0.1:13000/

[security]
admin_user = admin
admin_password = $__file{/etc/grafana/credentials/admin-password}
cookie_samesite = strict

[users]
allow_sign_up = false

[analytics]
reporting_enabled = false

[plugins]
allow_loading_unsigned_plugins = sub2api-jsoncopy-panel
INI

cat > /etc/grafana/provisioning/datasources/cloudwatch.yaml <<'YAML'
apiVersion: 1
datasources:
  - name: AWS CloudWatch
    type: cloudwatch
    uid: sub2api-cloudwatch
    access: proxy
    isDefault: true
    editable: false
    jsonData:
      authType: default
      defaultRegion: ap-southeast-1
      logsTimeout: 60s
YAML

install -d -m 0755 /var/lib/grafana/dashboards
cat > /etc/grafana/provisioning/dashboards/sub2api.yaml <<'YAML'
apiVersion: 1
providers:
  - name: Sub2API
    folder: Sub2API
    type: file
    updateIntervalSeconds: 15
    options:
      path: /var/lib/grafana/dashboards
YAML

systemctl enable --now grafana-server
curl -fsS --retry 20 --retry-connrefused --retry-delay 2 http://127.0.0.1:3000/api/health
