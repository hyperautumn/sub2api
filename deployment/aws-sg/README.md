# Sub2API 新加坡部署

- [应用基础设施](infra.yaml) 管理 VPC、ALB、WAF、EC2、RDS 与 Valkey。公网 API 由 ALB 转发，维护使用 SSM。
- [Grafana 基础设施](grafana/infra.yaml) 和 [仪表盘](grafana/dashboards.py) 提供流量、错误、时延和出口查询。部署使用参数存储和数据库只读用户。
- [环境变量样例](.env.shared.example) 配合 [AWS Compose 覆盖](compose.aws.yaml) 使用。数据库、Valkey 和节点参数由目标环境提供。
- [出口采集覆盖](compose.egress-capture.yaml) 使用已验证的 Linux 构建和指定测试 Key 的数字 ID。启用前在部署数据库应用 [采集表定义](grafana/egress-payloads.sql)。

## 合并上游版本

```bash
git switch custom
git fetch upstream
git merge upstream/main
```

全新克隆 fork 时先执行 `git remote add upstream https://github.com/Wei-Shaw/sub2api.git`。如按正式发行版升级，可合入上游对应 tag。

从合并后的提交构建包含前端的 Linux 镜像，并用目标架构和同版本数据库环境验收。发布前验证健康检查、额度保护返回 429、模型不存在返回 404，以及指定测试 Key 的 HTTP、SSE 和 WebSocket 出口采集。使用 Compose 的二进制挂载时，同步更新 `SUB2API_EGRESS_CAPTURE_BINARY` 指向已验收的构建。
