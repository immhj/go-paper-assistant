# Spec 0002 · 论文助手的 Web 聊天通道

> **Partially superseded：** [Spec 0003](0003-free-guest-access-email-verification.md) 与 [ADR 0016](../adr/0016-free-guest-access-and-email-verification.md) 将 Web 从账号专属改为同时接纳访客与账号，并新增访客资格、安全限流、导入/清理和成本熔断；[ADR 0017](../adr/0017-research-projects-as-web-conversation-boundary.md) 进一步将 Web 会话的直接归属改为研究项目，并以用户界面管理项目和会话结构。SSE、通道无关编排及 QQ 回归要求仍有效。本文原 `ready-for-agent` 状态仅作历史记录，不得脱离后续 Spec/ADR 独立执行。

> 状态：ready-for-agent（本仓库暂未配置 issue tracker，spec 以文件形式留存在 `docs/specs/`）。
> 依赖：Spec 0001（账号与登录态必须先就位）。相关 ADR：0013（Web 聊天通道以 SSE 复用编排，修订 0003）、0005（确定性编排）、0010（Router/Executor）。术语见根 `CONTEXT.md` 与 `docs/contexts/platform/CONTEXT.md`。

## Problem Statement

论文助手现在只能在 QQ 单聊里用，整个编排都绑在 QQ 上：入站是 `qq.IncomingMessage`，出站要 QQ 的 `Messenger` 回执，输出还受“对一条用户消息最多被动回复 4 次”的配额约束。平台已经有了登录账号（Spec 0001），但登录进来之后没有任何工具能真正用起来。我要让用户在网页里就能和论文助手对话——检索、看候选列表、精读——体验参考 GopherAI-v2 的流式聊天。

## Solution

给论文助手新增第二条入口通道：**Web 聊天通道**，与 QQ 单聊并列。用户在网页里发消息，后端通过 HTTP + SSE 逐帧 JSON 把回复流式推回，复用论文助手既有的确定性编排与 Router/Executor，不重写对话逻辑。两条通道的差别只在两点：归属键（Web 是平台账号，QQ 是匿名 QQ 用户）和配额约束（Web 不受 QQ 被动回复 4 次上限）。会话、候选列表、序号、展示记录、速览、精读这些论文助手的核心概念在两条通道里语义完全相同。

## User Stories

1. 作为一个登录的账号，我想在网页里给论文助手发一条消息并看到回复逐字流式返回，以便获得和 GopherAI-v2 一样顺滑的对话体验。
2. 作为一个账号，我想在网页里检索论文并看到带序号的候选列表，以便像在 QQ 里一样挑选论文。
3. 作为一个账号，我想在网页里对候选列表中的某一篇发起速览或精读，以便深入了解它。
4. 作为一个账号，我发起的会话只归属于我，别人看不到，以便我的研究方向和历史是私密的。
5. 作为一个账号，我想在网页里新建会话、切换会话、查看历史会话，以便同时保留多个独立的研究方向。
6. 作为一个账号，我的 Web 会话不受 QQ 那套“一条消息最多回复 4 次”的限制，以便一次检索的多段输出不会被配额截断。
7. 作为一个账号，当模型或数据源出错时，我在流里得到明确的错误提示而不是无声中断，以便知道发生了什么、可以重试。
8. 作为一个账号，我想在流式过程中看到“正在调用某工具”之类的进度提示，以便知道后端在干什么。
9. 作为一个账号，我发的消息和最终回复会被持久化，以便我下次回来还能看到历史。
10. 作为一个绑定了 QQ 的账号（未来能力），我希望 Web 会话与 QQ 会话能归到同一个人名下，以便两条通道的历史可以合并——本 spec 只保证数据模型支持，不实现合并视图。
11. 作为平台维护者，我希望 Web 通道复用现有编排，以便对话逻辑只维护一套。
12. 作为平台维护者，我希望 QQ 特有的推送细节（流分片 ID、序号、剩余长度、55 分钟兜底）不渗进编排核心，以便新通道不被 QQ 的限制污染。

## Implementation Decisions

- **引入通道无关的“回合” seam**：把当前 `handleQQ` 里从“路由一条用户输入”到“执行目标并产出回复”的逻辑，收敛成一个不依赖 QQ 的回合入口，签名约为 `RunTurn(ctx, ownerRef, conversation, userText, sink)`。`ownerRef` 表达“这个会话归谁”（账号或 QQ 用户），`sink` 是一个流式产出接口。QQ 和 Web 各自实现 `sink`，编排本身不再出现 `qq.IncomingMessage`。这是本 spec 的主 seam，也是主要工作量所在。
- **Sink 抽象出输出**：定义一个最小接口，语义类似
  ```
  type TurnSink interface {
      Delta(text string) error        // 文本增量
      Tool(name string) error         // 工具进度提示
      ShowPapers(shown []domain.ShownPaper) error // 候选列表/展示记录
      Fail(err error) error           // 错误
  }
  ```
  （形状示意，最终以实现为准。）QQ 的 sink 把这些映射到既有的流式消息 + 被动回复配额 + 展示记录持久化；Web 的 sink 把它们编码成 SSE 帧。展示记录（`shown_papers`）的写库发生在编排层，与通道无关。
- **归属键从 QQ 用户推广到“会话所有者”**：会话现在只按 `conversations.user_id`（QQ 用户）归属。Web 会话要按账号归属。决定给 `conversations` 增加一个可空 `owner_account_id`，与现有 `user_id` 二选一（恰有一个非空），用一条新迁移（预计 `0007`）落地；DAO 的会话查询与创建按“所有者”参数化。这样序号分配、候选列表快照、消息窗口等既有逻辑不必区分通道。
- **SSE 传输**：新增受 `jwtAuth` 保护的聊天接口，请求带会话标识与用户输入，响应 `Content-Type: text/event-stream`，逐帧 JSON：`{"content":"..."}`、`{"tool":"..."}`、候选列表帧、`[DONE]`、错误帧。选 SSE 而非 WebSocket（ADR 0013）：单向下推、能过 nginx、`gorilla/websocket` 仅留给 QQ 网关不外扩。新建会话可在流建立时先下发一帧会话标识（参考 GopherAI 的 `{"sessionId":...}`）。
- **不继承 QQ 配额**：Web 通道的 sink 不实现 `replyText` 里的 4 次上限检查；`stream_msg_id` / `msg_seq` / `remain_msg_len` / 55 分钟兜底是 QQ sink 的私事。编排层不得假设任何配额。
- **任务仍在进程内存**：Web 通道同样不承诺跨重启恢复（延续 ADR 0003 现状），需要时再统一上持久化任务/发件箱。
- **前端**：静态聊天页（对齐 GopherAI-v2 观感），用 `EventSource` 或 fetch-stream 消费 SSE，携带 JWT。会话列表、历史、新建/切换会话复用平台账号身份。

## Chat Progress, Provider Failures, and Duration (2026 amendment)

This amendment defines the Web chat experience that remains in scope after the guest/project supersessions above.

- **Observable task lifecycle.** After the server accepts a valid turn, it emits a `started` event and then zero or more `progress` events. A progress event carries a stable user-facing stage code and localized text such as “正在理解你的问题”, “正在规划任务”, or “正在执行：论文检索”. Progress reports an entered task stage, not hidden reasoning, provider prompts, credentials, or raw tool arguments.
- **Terminal events.** Text may arrive as `message`, `result`, or `delta` events. A terminal `error` event always has a stable code and safe user message, followed by `[DONE]`; the stream is never silently abandoned. `MODEL_BUSY` represents an upstream model `429` or `503` and says “模型服务当前繁忙，请稍后重试。”. `MODEL_TIMEOUT` represents an upstream model deadline and says “模型服务响应超时，请稍后重试。”. The server does not automatically retry either failure in this release: retrying would make wait time and cost unpredictable, so the user explicitly initiates another task instead.
- **Thinking state and duration.** The browser creates a temporary assistant bubble immediately after submission. It renders the latest progress text plus a live elapsed clock while the task is active; it removes or replaces that bubble on the first answer, terminal error, or stream completion. The connection indicator continues to report connection/synchronization state only. Every terminal answer and error displays the server-measured task duration. The same total and stage durations are persisted in the originating user message’s versioned Agent Trace and restored into chat history after reload.
- **SSE envelope.** The implemented JSON envelope is `{"status":"started|progress|message|result|delta|committed|completed|error", "code":"optional stable code", "content":"optional safe text", "elapsedMs":0}`. `completed` carries the final server-measured elapsed time after a successful text or delta response. `error` uses `error` for its safe display message; raw provider bodies remain server-side diagnostics. Existing consumers safely ignore unrecognized statuses.

## Testing Decisions

- **好测试只验外部行为**：对“给定一条用户输入 + 一个会话状态，回合产生了哪些 sink 事件、如何改动会话上下文”断言，不断言编排内部函数怎么拆。
- **主 seam：通道无关的回合入口（`RunTurn`）**。用 fake `TurnSink` + fake `dao.Store`/`PlatformStore` + fake `ModelClient` 驱动，覆盖：
  - 普通问答产出 `Delta` 序列，无候选；
  - 检索产出候选列表事件，序号分配正确、写入会话上下文；
  - 精读指代解析（“精读 #3”）命中正确论文；
  - 模型/数据源错误被映射为 `Fail` 而非静默结束；
  - 会话归属：账号所有者的会话不与 QQ 用户会话串号。
  这一个 seam 不涉及 QQ，也不涉及 HTTP。
- **次 seam：SSE handler（httptest）**。验证 sink 事件被正确编码为 SSE 帧、`[DONE]` 收尾、`jwtAuth` 未登录被拒。用 fake 回合逻辑或 fake sink 源，不重复测编排。
- **回归保护**：QQ 通道在重构后行为不变——把现有 `handleQQ` 路径改为经由 `RunTurn` + QQ sink 后，被动回复配额、流式分片、展示记录持久化仍与今日一致。若为 QQ 行为补测，放在同一回合 seam 上用 QQ sink 断言。
- **prior art**：无（见 Spec 0001）。本 spec 与 0001 共同建立测试基座；回合 seam 的 fake sink 可复用。

## Out of Scope

- **QQ 绑定的合并视图与绑定流程**：数据模型支持账号归属，但“把某账号绑到某 qq_openid 后合并展示两条通道历史”不在本期。
- **平台账号体系本身**：由 Spec 0001 提供，本 spec 假定其已就位。
- **持久化任务 / 跨重启恢复 / 发件箱**：延续现状不做。
- **多模型切换、图片理解、RAG、联网搜索**：这些是 GopherAI-v2 的能力，不是论文助手的领域，不随通道引入。
- **把 QQ 通道下线**：QQ 与 Web 并列长期共存，不替换。

## Further Notes

- 本 spec 的重构风险集中在“把编排从 QQ 解耦”这一步：改动面大且触及在跑的 QQ 主链路，建议先在回合 seam 上补齐 QQ 行为的回归测试，再动 Web。
- `conversations` 增列 `owner_account_id` 需要新迁移（`0007`），并相应更新 `docs/schema.sql` 里 conversations 的注释与归属说明。
- SSE 帧格式尽量与 GopherAI-v2 对齐，降低前端心智负担；但 `title` 自动命名等 GopherAI 特有能力属于论文助手已有的会话命名逻辑，按现状复用即可，不照搬其实现。
