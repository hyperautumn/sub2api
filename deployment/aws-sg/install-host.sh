#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
cloud-init status --wait

# The instance's outbound security group permits HTTPS.
if [[ -f /etc/apt/sources.list.d/ubuntu.sources ]]; then
  sed -i -E 's#http://[^/]+\.ec2\.archive\.ubuntu\.com/#https://archive.ubuntu.com/#g; s#http://(archive|security)\.ubuntu\.com/#https://\1.ubuntu.com/#g' \
    /etc/apt/sources.list.d/ubuntu.sources
fi
apt-get -o DPkg::Lock::Timeout=120 update -q
apt-get -o DPkg::Lock::Timeout=120 install -y -q ca-certificates curl jq unzip

if ! command -v aws >/dev/null; then
  cli_dir=$(mktemp -d)
  trap 'find "$cli_dir" -depth -delete' EXIT
  curl -fsSL --retry 3 https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o "$cli_dir/awscliv2.zip"
  unzip -q "$cli_dir/awscliv2.zip" -d "$cli_dir"
  "$cli_dir/aws/install"
fi

install -m 0755 -d /etc/apt/keyrings
curl -fsSL --retry 3 https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod 0644 /etc/apt/keyrings/docker.asc
. /etc/os-release
cat >/etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: ${UBUNTU_CODENAME:-$VERSION_CODENAME}
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/docker.asc
EOF

apt-get -o DPkg::Lock::Timeout=120 update -q
apt-get -o DPkg::Lock::Timeout=120 install -y -q docker-ce docker-ce-cli containerd.io docker-compose-plugin
systemctl enable --now docker
docker version --format 'Docker {{.Server.Version}}'
docker compose version
aws --version
