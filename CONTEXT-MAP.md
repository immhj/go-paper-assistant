# 上下文地图

本仓库最初只是一个通过 QQ 单聊使用的**论文助手**。从现在起它成长为一个面向研究生的免费**工具集平台**：平台允许访客有限使用，并为完成邮箱验证的账号提供持久身份、鉴权和导航；论文助手是接入平台的第一个工具，后续还会有别的工具按同样方式挂进来。

这带来两个互不相同的限界上下文。它们的词汇不能混用——最容易踩的坑是「用户」：在论文助手里「用户」一直指匿名的 QQ 用户；平台里的未登录主体叫「访客」，持久登录身份叫「账号」。三者是不同的对象，可以属于同一个自然人，也可以毫无关系。

## 上下文

### 平台（研究生工具集）
访客、账号、邮箱验证、昵称、内部用户名、JWT 鉴权、Web 聊天通道、工具导航，以及统一的管理后台。这是新增的上下文。
- 术语表：[`docs/contexts/platform/CONTEXT.md`](docs/contexts/platform/CONTEXT.md)

### 论文助手
论文的发现与两级分析，以及 Web 端研究项目对多个隔离会话、项目论文和精读结果的组织：会话、项目上下文、检索规格、候选列表、序号、展示记录、速览、精读、相关性、质量。
- 术语表：[`CONTEXT.md`](CONTEXT.md)

## 两者的接缝

- **归属**：论文助手的 QQ 会话仍按 `users.qq_openid` 对应的 User 归属。Web 会话归属一个私有研究项目，研究项目再恰好归属平台 Account 或 Guest；一个 Account 或 Guest 可以拥有多个项目。Account 可选绑定 `qq_openid` 以合并查看两条通道的历史，但 QQ 会话首版不进入研究项目。
- **通道**：论文助手现在有两条入口通道——QQ 单聊（原有，ADR 0003）与 Web 聊天（新增，ADR 0013）。它们复用同一套确定性编排，只是归属键和配额约束不同。
- **系统级决策**记在根 `docs/adr/`；两个上下文共享同一套 ADR 序号，不各自另起。

## 相关 ADR

- `0012` 从单通道 QQ 机器人转向研究生工具集 Web 平台
- `0013` Web 聊天通道以 SSE 复用编排（修订 0003；身份与额度结论由 0016 部分取代）
- `0014` 平台账号身份与 JWT 鉴权（邮箱验证与 JWT 撤销结论由 0016 部分取代）
- `0015` 邀请码准入（已由 0016 取代，仅保留历史）
- [`0016`](docs/adr/0016-free-guest-access-and-email-verification.md) 免费产品、访客访问与邮箱验证
- [`0017`](docs/adr/0017-research-projects-as-web-conversation-boundary.md) 研究项目作为 Web 会话归属与共享研究边界（“首版不提供文件上传”由 0019 部分取代）
- [`0018`](docs/adr/0018-ieee-source-via-openalex-doi-identity.md) 通过 OpenAlex 提供 IEEE 数据源，以 DOI 作为外部身份
- [`0019`](docs/adr/0019-account-private-full-text-upload.md) 账号上传 PDF 建立私有全文（平台账号与论文助手的接缝）
- [`0020`](docs/adr/0020-deterministic-venue-rank-matching.md) 发表场所等级只做确定性匹配，并固定第三方数据版本
- [`0021`](docs/adr/0021-bounded-plan-execution-on-web.md) Web 通道有界计划执行（部分取代 0010）

## 相关 Spec

- `0001` 平台基座（邀请码、邮箱验证、JWT 撤销、限流/熔断部分由 0003 取代）
- `0002` Web 聊天通道（账号专属与额度部分由 0003 取代，Web 会话直接归属由 0004 取代）
- [`0003`](docs/specs/0003-free-guest-access-email-verification.md) 免费访客访问与邮箱验证（Web 会话/导入归属部分由 0004 取代；其余要求继续有效）
- [`0004`](docs/specs/0004-research-projects.md) 研究项目、项目论文与精读历史（`ready-for-agent`）
