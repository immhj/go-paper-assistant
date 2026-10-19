# 公网部署计划与检查清单

目标：国内云单机，`https://<公网IP>` 对外演示并开放给实验室成员。相关决策见 ADR 0022（IP 证书 TLS）、ADR 0023（后台回环 listener）。

## 实施顺序

每项一个分支 + 一个 PR，CI 通过后合并。1–5 完成前不上公网。

1. `git init`，确认 `.env`、`data/backups/*`、`data/fulltext/*` 未被跟踪；若 `.env` 中的密钥曾外泄，全部轮换。
2. Fail-closed 配置：`APP_ENV` 只接受 `development|production`，compose 默认 `production`；production 下 `ADMIN_PASSWORD` ≥ 16 位；数值/时长解析失败即启动失败；app 服务加 healthcheck。
3. 后台与 `/metrics` 拆到 `127.0.0.1` 管理 listener（ADR 0023）。
4. 限流：
   - 登录失败按邮箱（5 次 / 15 分钟）和 IP（20 次 / 15 分钟）计数，超限返回 429；
   - 未知邮箱执行假 argon2 哈希；
   - argon2 并发信号量（4）；
   - 所有 `/api/*` 按 IP 60 次 / 分钟。
5. 成本护栏：Redis `appendonly yes`、`appendfsync everysec`；DeepSeek 请求设置 `max_tokens`；`PLATFORM_COST_HOURLY_USD=1`、`PLATFORM_COST_DAILY_USD=5`。
6. HTTP 服务补 `ReadTimeout`、`IdleTimeout`（SSE 不设全局 `WriteTimeout`）；slog 改 JSON，日志带 `request_id`、`status`、`client_ip`。
7. Prometheus `/metrics`：请求延迟、LLM token 与成本、限流命中、访客创建数。
8. GitHub Actions：`go vet`、`go test -race ./...`、golangci-lint。
9. 部署物料：`docker-compose.prod.yml`、`Caddyfile`、`scripts/release.ps1`、备份脚本。
10. 上线后 k6 压测（SSE 并发、限流生效），结果写入报告。

## 服务器准备

- 安全组只开放 22、80、443。
- SSH 只允许密钥登录，禁用密码登录和 root 登录。
- 先验证 80 端口外网可达：`sudo python3 -m http.server 80`，从外网访问。不可达则按 ADR 0022 第 4 条降级。
- MySQL、Redis 使用强随机密码，不发布到宿主机端口。
- `.env` 权限设为 `600`，只存在于服务器上。

## 发布（本机打包）

`scripts/release.ps1` 依次执行：

1. `docker build --platform linux/amd64` 构建 app 和 paperscraper，镜像标签为 git 短 SHA；
2. `docker save` 后压缩；首次发布同时打包 mysql、redis、caddy 基础镜像；
3. `scp` 上传到服务器；
4. `ssh` 到服务器执行 `docker load` 和 `docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d`；
5. 检查 `/healthz`。

回滚：`release.ps1 -Rollback <sha>`。服务器保留最近 3 个版本的镜像。

## 备份

- 每日 cron：`mysqldump --single-transaction`，连同 `fulltext-data` volume 一起打包，本机保留 7 天，并上传到阿里云 OSS。
- 每月做一次恢复演练：在空库上导入，确认能正常启动。

## 上线前核对

- [ ] `APP_ENV=production`，应用能正常启动（即配置校验通过）
- [ ] `https://<IP>` 显示有效证书；`http://<IP>` 跳转到 https
- [ ] 公网访问 `/admin/`、`/api/admin/*`、`/metrics` 均返回 404
- [ ] 通过 SSH 隧道可以打开后台
- [ ] 连续登录失败会返回 429
- [ ] 访客 cookie 在 https 下能保持（刷新后访客 ID 不变）
- [ ] 重启 Redis 后当日成本计数仍在
- [ ] 备份文件已出现在 OSS
- [ ] 证书到期时间大于 3 天（每周检查一次，或加监控告警）
