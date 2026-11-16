#!/usr/bin/env bash
set -euo pipefail
export AWS_DEFAULT_REGION=ap-southeast-1 AWS_PAGER=''
database_host=$1

install -d -m 0750 -o root -g grafana /etc/grafana/credentials
curl -fsSL --retry 3 https://truststore.pki.rds.amazonaws.com/ap-southeast-1/ap-southeast-1-bundle.pem \
  -o /etc/grafana/credentials/rds-ca.pem
chmod 0644 /etc/grafana/credentials/rds-ca.pem
database_password=$(aws ssm get-parameter --name /sub2api/grafana/reader-password --with-decryption \
  --query Parameter.Value --output text)
[[ "$database_password" =~ ^[0-9a-f]{48}$ ]]
umask 027
cat > /etc/grafana/provisioning/datasources/postgres.yaml <<YAML
apiVersion: 1
datasources:
  - name: Sub2API PostgreSQL (read only)
    type: grafana-postgresql-datasource
    uid: sub2api-postgres
    access: proxy
    editable: false
    url: "${database_host}:5432"
    user: grafana_sub2api_reader
    jsonData:
      database: sub2api
      sslmode: verify-full
      tlsConfigurationMethod: file-path
      sslRootCertFile: /etc/grafana/credentials/rds-ca.pem
      postgresVersion: 1500
      maxOpenConns: 4
      maxIdleConns: 2
      maxIdleConnsAuto: false
    secureJsonData:
      password: "$database_password"
YAML
chown root:grafana /etc/grafana/provisioning/datasources/postgres.yaml
chmod 0640 /etc/grafana/provisioning/datasources/postgres.yaml
