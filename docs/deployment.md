# 公网部署手册

目标：国内云单机（建议 2C4G），`https://<公网IP>` 对外演示并开放给实验室成员。决策依据见 ADR 0022（IP 证书 TLS）、ADR 0023（后台回环 listener）。

## 拓扑

```
互联网 ──80/443──▶ caddy (172.30.0.10, IP 证书, 6 天自动续期)
                     │ http://app:8080   内部网络 172.30.0.0/24
                     ▼
                    app ──▶ mysql / redis / paperscraper（均不发布到宿主机）
                     │
           127.0.0.1:9090  后台 + /metrics，只能经 ssh -L 访问
```

相关文件：

| 文件 | 作用 |
|---|---|
| `docker-compose.prod.yml` | 生产叠加：加 Caddy、撤掉 app/MySQL 的宿主机端口、固定子网、内存上限、日志轮转、强制 Redis/MySQL 密码 |
| `deploy/Caddyfile` | IP 证书（`profile shortlived`）、HSTS、SSE 不缓冲、边缘再拦一次 `/admin` `/metrics` |
| `deploy/.env.production.example` | 服务器 `.env` 模板 |
| `deploy/remote.sh` | 服务器端：加载镜像、切换版本并等待健康、清理旧版本 |
| `deploy/backup.sh` | 每日备份 MySQL 与全文卷，可选上传 OSS |
| `scripts/release.ps1` | 本机：构建 → `docker save` → scp → 远端加载并切换 |

## 一、服务器准备（一次）

1. **安全组**只放行 22、80、443（443 同时放行 UDP，用于 HTTP/3）。
2. **验证 80 端口外网可达**，这是 IP 证书的前提：`sudo python3 -m http.server 80`，从手机流量访问 `http://<IP>`。不通则按 ADR 0022 第 4 条降级。
3. **SSH 加固**：建 `deploy` 用户并加入 `docker` 组，配置公钥登录；`/etc/ssh/sshd_config` 设 `PasswordAuthentication no`、`PermitRootLogin no`，重启 sshd 前先另开一个会话确认能登录。
4. **安装 Docker Engine 与 compose 插件**（用云厂商镜像源）。
5. **准备目录和 `.env`**：
   ```bash
   sudo mkdir -p /opt/paper-assistant && sudo chown deploy: /opt/paper-assistant
   # 把 deploy/.env.production.example 的内容填好后保存为 /opt/paper-assistant/.env
   chmod 600 /opt/paper-assistant/.env
   ```
   密钥用 `openssl rand -base64 48 | tr -d '/+=' | cut -c1-40` 生成。`MYSQL_DSN` 里的密码必须与 `MYSQL_PASSWORD` 一致。

## 二、发布

在本机（Windows PowerShell）仓库根目录：

```powershell
./scripts/release.ps1 -Server deploy@<IP> -PublicIP <IP> -IncludeBaseImages   # 首次：连同 mysql/redis/caddy 一起带上
./scripts/release.ps1 -Server deploy@<IP> -PublicIP <IP>                      # 之后：只带 app 和 paperscraper
./scripts/release.ps1 -Server deploy@<IP> -Rollback <sha>                     # 回滚到服务器上已有的版本
```

普通发布要求工作区干净，并通过当前 **HEAD** 对应的 GitHub Actions `CI` workflow；脚本不会接受分支上某次更早的绿色构建。仅紧急情况可显式加 `-SkipCI` 绕过，终端会给出警告。镜像标签取 git 短 SHA；`remote.sh activate` 在 app 健康检查通过前不会返回成功。

切换成功后，脚本会建立一次临时 SSH 隧道到远端 Caddy 的回环 443，再对 `https://<IP>/healthz` 做 TLS 校验完整的只读检查。该检查不创建访客/项目记录，也不调用模型。它失败时**不会自动回滚**：SSH 或本机端口问题不应触发破坏性操作；脚本会打印上一个镜像 tag 对应的显式回滚命令。正常发布后服务器只保留最近 3 个 tag，因此太旧的版本可能已无法回滚。

**首次发布后初始化数据库**：`docs/schema.sql` 只在 MySQL 数据卷为空时自动执行；之后的结构变更按 `docs/migrations/` 手工执行，执行前先备份。

## 三、备份

```bash
crontab -e
30 3 * * * /opt/paper-assistant/deploy/backup.sh >> /opt/paper-assistant/backups/backup.log 2>&1
```

- 内容：`mysqldump --single-transaction` + `fulltext` 卷，打成一个 tar，本机保留 7 天。
- `.env` 里设置 `OSS_BUCKET` 并配置好 `ossutil` 后自动上传阿里云 OSS。
- 每月做一次恢复演练：新建空 MySQL，导入 `mysql.sql.gz`，确认应用能启动。

## 四、日常运维

```bash
ssh -L 9090:127.0.0.1:9090 deploy@<IP>     # 然后打开 http://127.0.0.1:9090/admin/
bash /opt/paper-assistant/deploy/remote.sh status
docker compose -f docker-compose.yml -f docker-compose.prod.yml logs -f --tail 100 app
curl -s http://127.0.0.1:9090/metrics | grep paperassistant_rate_limited_total
```

## 五、上线核对

- [ ] `remote.sh status` 显示所有服务 healthy，`IMAGE_TAG` 为本次 SHA
- [ ] `https://<IP>` 证书有效（签发方 Let's Encrypt，有效期约 6 天）；`http://<IP>` 跳转到 https
- [ ] 公网访问 `/admin/`、`/api/admin/overview`、`/metrics` 均为 404
- [ ] 经 SSH 隧道能打开后台
- [ ] app 日志里 `client_ip` 是你的公网 IP，而不是 `172.30.0.10`（验证 `TRUSTED_PROXY_CIDRS` 生效）
- [ ] 同一邮箱连续 6 次错误密码，第 6 次返回 429
- [ ] 刷新页面后访客会话保持（Secure cookie 在 https 下生效）
- [ ] 一次 SSE 对话逐字流式显示，而不是最后一次性出现（Caddy `flush_interval -1` 生效）
- [ ] 手动执行一次 `backup.sh`，`backups/` 下出现归档（启用 OSS 时 OSS 里也有）
- [ ] 证书剩余有效期大于 3 天；之后每周检查一次，或用外部监控对 `https://<IP>` 做证书到期告警

## 后续计划

- k6 压测：SSE 并发与限流生效情况，结果写入报告。
- 成本先预扣后结算，消除并发请求超过成本上限的窗口。
- `dao/mysql` 用 testcontainers 补集成测试。
