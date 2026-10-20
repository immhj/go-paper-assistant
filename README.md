# go-paper-assistant

[![CI](https://github.com/immhj/go-paper-assistant/actions/workflows/ci.yml/badge.svg)](https://github.com/immhj/go-paper-assistant/actions/workflows/ci.yml)

面向研究生的论文检索与精读助手：用一句话描述研究问题，系统检索 arXiv / IEEE 候选论文、按你的方向判断相关性并标注 CCF / JCR 等级，需要时基于**完整全文**做快速精读、深度精读、多篇对比和项目内研究问答。提供 Web（SSE 流式）和 QQ 官方机器人两条入口，共用同一套编排。

LLM-powered research paper search & deep-reading assistant — Go service, Python scraping sidecar, Web (SSE) + QQ Bot.

## 架构

```
                       ┌──────────── 127.0.0.1:9090 (ssh -L) ── 管理后台 / Prometheus /metrics
                       │
浏览器 ──HTTPS──▶ Caddy ──▶ Go app ──┬── MySQL 8.4   账号、研究项目、会话、论文、精读结果
QQ 单聊 ◀─WebSocket 网关─────┘      ├── Redis 7.4   限流、验证码、成本熔断、并发租约（AOF）
                                     ├── paperscraper (Python)  arXiv / OpenAlex(IEEE) 检索、场所等级
                                     ├── pdftotext   全文抽取与质量校验
                                     └── DeepSeek    规划、相关性判断、精读（流式）
```

| 目录 | 内容 |
|---|---|
| `cmd/server` | 进程入口：依赖装配、公网与管理两个 listener、优雅退出 |
| `internal/router`, `internal/controller` | 路由、鉴权中间件、SSE、访问日志与指标 |
| `internal/service` | 编排：有界计划执行、检索、精读、研究项目、访客/账号平台 |
| `internal/provider/*` | DeepSeek、paperscraper、全文抽取、邮件、验证码、限流、成本守卫 |
| `internal/dao/mysql` | 存储实现（`database/sql`，无 ORM） |
| `services/paperscraper` | Python sidecar：数据源适配、配额、CCF / JCR / 中科院分区确定性匹配 |
| `docs/adr` | 23 篇架构决策记录 |

## 设计要点

- **有界的 LLM 编排，而非开放式 agent 循环**（[ADR 0021](docs/adr/0021-bounded-plan-execution-on-web.md)）：一次规划调用产出最多 3 步的计划，由 Go 的转移表校验后执行；执行中不把结果回传模型重新规划，单轮调用次数和成本上限可事先算出。
- **全文进上下文，不做 RAG**（[ADR 0001](docs/adr/0001-full-text-into-context-no-rag.md)）：精读依据完整全文，全文质量校验作为防幻觉闸门。
- **身份与访问**：argon2id 密码哈希；HS256 JWT 固定算法并以 `auth_version` 支持吊销；访客凭证只存哈希；邮箱验证码带 pepper、限次、Lua 原子状态机（[ADR 0014](docs/adr/0014-account-identity-and-jwt.md)、[0016](docs/adr/0016-free-guest-access-and-email-verification.md)）。
- **研究项目**作为 Web 会话的归属边界，项目间上下文完全隔离（[ADR 0017](docs/adr/0017-research-projects-as-web-conversation-boundary.md)）。

## 上线前的安全加固

部署到公网前做了一轮自审，每项一个提交：

| 问题 | 处理 |
|---|---|
| `APP_ENV` 漏配会静默启用硬编码 JWT 密钥和 `admin/admin` | 配置 fail-closed：默认 production、只接受两个取值、解析失败即拒绝启动 |
| 后台与公共 API 同端口，Basic Auth 凭证随每个请求过公网 | 后台和 `/metrics` 拆到只绑回环的独立 listener（[ADR 0023](docs/adr/0023-admin-and-metrics-on-loopback-listener.md)） |
| 登录无限流；argon2 每次 64 MiB 且无并发上限；未知邮箱不做哈希可被计时枚举 | 按邮箱 + IP 的失败锁定（429 + `Retry-After`）、4 槽并发信号量、假哈希等时 |
| Redis 无持久化，重启即清零成本熔断；模型调用无 `max_tokens` | AOF 持久化、每次调用固定输出上限、截断可观测 |
| 无域名、无法备案，只能用 IP | Caddy + Let's Encrypt IP 证书（6 天短期证书自动续期，[ADR 0022](docs/adr/0022-tls-via-ip-certificate-without-domain.md)） |

## 可观测性

- JSON 结构化访问日志：`request_id`（同时写入 `X-Request-ID` 响应头）、`status`、`duration_ms`、`client_ip`（只信任可信代理的 XFF）；不记录 query string。
- Prometheus 指标：按**路由模式**（而非原始路径）统计请求量与延迟，避免 ID 撑爆基数；LLM token / 花费 / 截断；各限流器拒绝次数；访客创建数。

## 本地运行

需要 Docker（Compose v2.24+）。

```powershell
Copy-Item .env.example .env     # 本地开发保持 APP_ENV=development
docker compose up -d --build
```

- Web：<http://127.0.0.1:8080/app/>（端口见 `.env` 的 `APP_PORT`）
- 管理后台：<http://127.0.0.1:9090/admin/>
- 指标：<http://127.0.0.1:9090/metrics>

不填 `DEEPSEEK_API_KEY` 也能启动，模型相关功能会降级。

## 测试

日常改动先跑与改动范围对应的测试；提交前运行完整确定性检查。CI 会在每个 push 和 PR 自动运行相同检查，并额外启动真实 MySQL、Redis 和整套 Compose 服务。

```powershell
# Go：格式、静态检查、单测与覆盖率（覆盖率仅展示，不设阈值）
gofmt -l ./cmd ./internal
go vet ./...
go test -count=1 -coverprofile coverage.out ./...
go tool cover -func coverage.out

# Go race：Windows 本机若未安装 C 编译器，用 Linux 容器运行
docker run --rm -v "${PWD}:/src" -w /src golang:1.26-bookworm sh -c "go test -race -count=1 ./..."

# 安全与前端语法
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
node scripts/check-inline-js.mjs

# Python sidecar（先安装 requirements，或在其镜像中运行）
cd services/paperscraper; python -m unittest discover -v
```

真实 MySQL 集成测试需要一个**专用、可创建和删除测试库**的 MySQL 实例，绝不能使用 `MYSQL_DSN` 指向的开发或生产库：

```powershell
$env:MYSQL_TEST_ADMIN_DSN='root:password@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=true&loc=UTC'
go test -race -count=1 -tags=integration ./internal/dao/mysql
```

该测试会为每个用例创建随机 schema、执行权威的 `docs/schema.sql`、验证 production store 启动兼容性，测试结束后只删除自己创建的 schema。覆盖访客精读事务幂等性、项目导入编号重分配与并发编号分配。

`cmd/smoke` 是 E2E 冒烟程序。CI 会以独立 Compose 项目启动 MySQL、Redis、sidecar 和 app，并在**不配置模型密钥**的条件下验证：健康检查、访客 cookie、空会话、SSE 降级回复和自动创建的当前项目。对已运行的隔离实例可执行：

```powershell
$env:SMOKE_BASE_URL='http://127.0.0.1:18080'
go run ./cmd/smoke
```

发布脚本使用 `-health-only` 与 `-connect-addr` 走 SSH 隧道验证生产 Caddy HTTPS 路径；该模式只读 `GET /healthz`，不创建访客记录、不产生模型调用。

模型评测与真实模型调用不在普通 CI 中运行。`Live Model Evaluation` workflow 仅在 main 的规划/相关性提示词或种子用例变更后、或手动触发时执行；需在 GitHub Actions secrets 中配置 `DEEPSEEK_API_KEY`。它运行 50 条人工种子用例、每次预留最高 $0.01 且总预算为 $0.20，结果写入 Job Summary 并上传 artifact，但**不阻断合并或发布**。见 [ADR 0024](docs/adr/0024-ci-gates-and-non-blocking-llm-eval.md)。

## 部署

单机 Docker Compose + 本机构建离线发布，见 [docs/deployment.md](docs/deployment.md)。

## 文档

- 领域术语：[CONTEXT.md](CONTEXT.md)、[平台术语](docs/contexts/platform/CONTEXT.md)、[上下文地图](CONTEXT-MAP.md)
- 架构决策：[docs/adr](docs/adr)
- 功能规格：[docs/specs](docs/specs)
