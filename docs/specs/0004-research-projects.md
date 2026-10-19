# Spec 0004 · 研究项目、项目论文与精读历史

> 状态：ready-for-agent
> 相关 ADR：[0017 研究项目作为 Web 会话归属与共享研究边界](../adr/0017-research-projects-as-web-conversation-boundary.md)。本文取代 Spec 0002/0003 中 Web 会话直接使用 `owner_account_id` / `owner_guest_id`、转移独立 Web 会话和保留迁移前 Web 会话的要求；Spec 0003 的访客凭证、邮箱验证、精读资格、安全限流、成本熔断与 SSE 要求继续有效。
> 术语与边界：以 [`CONTEXT-MAP.md`](../../CONTEXT-MAP.md)、[平台术语表](../contexts/platform/CONTEXT.md)和[论文助手术语表](../../CONTEXT.md)为准。“账号”“访客”“QQ 用户”不得互换。

## 1. 目标与范围

交付一个私有、服务端持久化的研究项目模型：账号或访客拥有多个项目，项目拥有多个相互隔离的 Web 会话和可复用的项目论文，项目论文展示全部快速/深度精读历史。QQ 会话继续直属 QQ 用户，不进入项目。

以下“必须”“不得”均为上线验收条件。项目是 MySQL 逻辑结构，不是本地或云端文件夹；不保存项目专属 PDF、全文副本或任意用户文件。项目、会话和项目论文数量不设置产品次数额度，但仍受安全限流、并发、长度、分页、成本熔断和人工急停约束。

## 2. 归属与上下文不变量

### R1. 项目与会话归属

1. `projects` 每行必须恰好设置 `owner_account_id` 或 `owner_guest_id` 之一。项目完全私有；所有查询先由服务端身份解析 owner，再在 owner 范围内解析 `project_no`，不得接受客户端提供的全局 owner ID。
2. `conversations` 每行必须恰好设置 `user_id` 或 `project_id` 之一。Web 会话只设置 `project_id`；QQ 会话只设置 `user_id`。Web API 不得读取 QQ 会话，QQ 路径不得创建或选择项目会话。
3. `project_no` 在单个账号或访客范围内稳定唯一；`conversation_no` 在单个项目或 QQ 用户范围内稳定唯一。标题可重复，改名、归档、恢复和项目导入均不改变既有编号；导入时仅允许为解决目标账号冲突重新分配 `project_no`。
4. 会话创建后固定属于原项目，首版不支持跨项目移动。项目、会话、项目论文树只能由 Web UI 对应的确定性 API 修改；Agent 无权创建、选择、改名、归档、恢复、加入或移出。
5. `accounts.active_project_id`、`guest_sessions.active_project_id` 和 `projects.active_conversation_id` 均允许为 NULL。由于外键不能表达“当前目标必须属于同一 owner/project”，这些循环指针不要求 FK，但每次写入必须在同一事务中锁定 owner/project 并校验归属与 `active` 状态。

### R2. 会话上下文与项目上下文

1. 会话上下文只包含该会话的研究方向、检索规格、最近用户原话、候选列表、稳定论文序号和展示记录。同一项目中的会话不得共享普通聊天、方向、候选列表、序号或滑动窗口。
2. 项目上下文只包含该项目的项目论文和精读结果关系。项目内容不得整体自动注入模型；其他会话的普通消息永远不得经项目工具暴露。
3. 精读历史是模型旧输出，可供用户查看和 Agent 定位已有工作，但不得作为新事实性结论的唯一证据。新的比较或研究回答必须重新读取论文摘要或 `fulltext_status='ok'` 的校验全文，并把旧结果标示为“历史输出，待当前证据核对”。
4. 所有项目、会话、项目论文和结果查询必须执行 owner-scoped 过滤。跨 owner 或跨项目目标对外统一表现为不存在，不得泄露全局 ID、标题或存在性。

## 3. 项目与会话生命周期

### R3. 懒创建与选择

1. 账号或访客仅访问静态页面、登录、身份查询或任何 GET 列表时不得创建项目。只有调用 `POST /api/projects/ensure-active` 或 `POST /api/chat` 且没有 active 项目时，才在短事务中创建 `project_no=下一个编号`、标题“默认项目”的 active 项目及其 `conversation_no=1`、标题“默认会话”的 active 会话，并同时写入两个 active 指针。
2. 用户显式新建项目时，必须在同一事务中分配 owner 范围内下一个 `project_no`、创建项目、创建该项目默认会话，并把新项目和默认会话设为当前；任一步失败全部回滚。
3. 切换项目时，目标必须 active。若其 `active_conversation_id` 指向该项目的 active 会话，则恢复该会话；否则将项目指针清为 NULL。仅切换项目不自动恢复已归档会话。
4. 在 active 项目内发送聊天而其 active 会话为空时，短事务创建该项目下一个 `conversation_no` 的“默认会话”并设为当前。显式创建会话同样分配稳定编号并设为当前。

### R4. 项目归档与恢复

1. 项目只能软归档或恢复，首版不提供账号/访客永久删除项目。归档项目不删除其会话、消息、项目论文或精读历史，也不允许聊天、结构修改、加入论文或 Agent 项目工具读取。
2. 归档当前项目时，在同一事务中把项目设为 `archived`，并把 owner 的当前项目回退到最近更新的其他 active 项目（`updated_at DESC, id DESC`）。回退项目已有有效 active 会话时恢复该指针；没有时保留 NULL，下一次聊天懒建默认会话。
3. 没有其他 active 项目时，owner 的 active 项目指针设为 NULL；归档响应不得立即制造项目。下一次真正进入项目功能或发送聊天时，按 R3 懒建新的默认项目和默认会话。
4. 归档非当前项目不得改变 owner 当前项目。恢复项目不改变编号；若 owner 当前项目为空，恢复的项目成为当前，否则只恢复不切换。恢复不会自动恢复该项目内已归档会话。
5. 项目列表默认只列 active；归档视图显式请求 `status=archived`。归档项目在 UI 中只读展示，并提供恢复入口。

### R5. 会话归档与恢复

1. Web 会话只能在原项目内软归档或恢复。归档不删除消息、候选序号或展示记录。
2. 归档当前会话时，项目指针回退到同项目最近更新的其他 active 会话（`updated_at DESC, id DESC`）。归档项目中最后一个 active 会话是合法操作：`projects.active_conversation_id` 设为 NULL，不立即创建替代会话；下一次在该项目聊天时懒建“默认会话”。
3. 归档非当前会话不改变项目指针。恢复会话不改变编号；若项目当前会话为空且项目 active，则恢复的会话成为当前，否则只恢复不切换。
4. QQ 的 `/new`、`/list`、`/use`、`/current` 和归档语义保持既有行为，仅按 `user_id` 查询，绝不经过项目 API。

## 4. 项目论文与精读历史

### R6. 项目论文

1. `project_papers` 以 `(project_id,paper_id)` 唯一引用全局 `papers`，状态为 `active|removed`。加入论文使用 upsert：不存在则创建，已软移出则恢复，已 active 则幂等成功。
2. 普通检索和速览只产生当前会话候选，不自动加入项目。用户可从论文卡片明确加入；快速精读或深度精读成功提交时必须自动创建或恢复项目论文。失败、取消或未提交的精读不得加入。
3. “移出项目”只把状态改为 `removed`，不删除关系、全局论文、全文缓存、精读历史、消息或会话序号。再次加入或再次成功精读恢复同一行及全部历史。
4. 项目论文不分配 `#N`。UI 使用论文卡片和稳定 `paperId` 操作；自然语言标题匹配到零篇返回不存在，匹配到多篇必须澄清，不能任意选择。
5. active 项目论文在项目资料区展示；removed 论文仅在“已移出”视图展示。两种视图都可查看历史，只有 active 项目论文可被 Agent 项目工具选取。

### R7. 精读结果历史

1. `project_read_results` 只保存 `project_paper_id`、已持久化 bot `message_id`、`source_conversation_id`、`read_type=quick|deep` 和生成时间，不复制消息正文。
2. 每个 bot 精读消息最多对应一条结果关系。创建关系时必须验证：消息 role 为 bot；消息属于 `source_conversation_id`；来源会话和项目论文属于同一当前项目；消息已成功持久化。
3. 历史永久保留到 owner 生命周期清除或项目级联删除。默认按 `created_at DESC, id DESC` 展示最新结果，UI 首先展开最新一条，并标明类型、来源会话与生成时间。
4. 项目论文软移出、会话归档或项目归档不删除结果。消息正文仍从 `messages` 读取；关系中不得保存摘要副本以规避双写漂移。

## 5. HTTP API 与分页

所有 JSON 错误沿用 `{ "error": "...", "status": "ERROR_CODE" }`；客户端只依赖稳定 `status`。改变状态的 API 必须执行既有同源/请求标记与 CSRF 防护。下面的 `{projectNo}` 必须在当前账号/访客范围解析，`{conversationNo}` 必须在已解析项目范围解析；不得把数据库主键作为可枚举路由键。

### 5.1 通用资源与分页契约

1. 资源对象固定为：
   - `Project={projectNo,title,status,selected,activeConversationNo,createdAt,updatedAt}`；
   - `Conversation={conversationNo,title,status,selected,createdAt,updatedAt}`；
   - `ProjectPaper={paperId,title,publicationDate,venueRaw,status,latestReadResult:null|ReadResultMeta,updatedAt}`；
   - `ReadResultMeta={readType,sourceConversationNo,createdAt}`；完整历史项为 `ReadResultMeta+{content}`，其中 `content` 实时读取对应 bot message，不在结果关系重复存储。
2. Spec 0003 的 `GET /api/session` 响应新增 `research:{activeProjectNo:null|number,activeConversationNo:null|number}`，这是 owner 当前选择的权威读模型。项目/会话列表对象的 `selected` 必须与它一致；SSE 断线和页面恢复先重取该接口。
3. 所有列表使用 keyset cursor：请求 `limit` 默认 20、最小 1、最大 100，`cursor` 为服务端不透明字符串；响应统一为 `{items:[...],nextCursor:string|null}`。
4. 项目、会话和项目论文按 `updated_at DESC,id DESC`；精读历史按 `created_at DESC,id DESC`。cursor 必须绑定 actor、端点、父资源、status 过滤和最后排序键，不能跨列表复用。
5. 这是明确的最终一致分页，不是数据库快照：keyset 只保证未变化行不因 OFFSET 位移而重复或跳过；翻页期间改名、归档、恢复或软移出会改变 `updated_at`/过滤集合，客户端收到 mutation 或 `RESOURCE_STATE_CONFLICT` 后必须丢弃旧 cursor 并从第一页重取。服务端不得为追求快照持有跨请求事务。
6. 非法、篡改、过期或过滤条件不匹配的 cursor 返回 `400 INVALID_CURSOR`。`status` 只接受端点声明的枚举；省略时为 `active`。列表不接受无上限返回。

### 5.2 懒创建触发器

1. 只有 `POST /api/projects/ensure-active` 和 Web 聊天 `POST /api/chat` 可以调用同一个 `ensureActiveProject` 事务；前者表示用户明确进入研究工作区，返回 `200 {created:boolean,project:Project,conversation:null|Conversation}`。没有 active 项目时原子懒建默认项目/会话；有 active 项目但没有 active 会话时，只有 `/api/chat` 继续懒建默认会话，`ensure-active` 返回 `conversation:null`。
2. `GET /api/session`、`GET /api/projects`、归档列表、会话/论文/历史读取、登录和静态页面初始化全部只读，绝不懒建资源。显式 `POST /api/projects` 和 restore/select mutation 按各自契约创建或选择，不调用隐式创建。
3. 因此归档最后一个项目后的 UI 先显示空态；用户明确进入工作区可调用 `ensure-active`，直接发送聊天也会在服务端先执行相同懒创建。归档最后一个会话后，只有下一次聊天懒建该项目默认会话。

### 5.3 项目 API

- `GET /api/projects?status=active|archived&limit=&cursor=`：返回 `{items:Project[],nextCursor}`；每项含 `selected`，与 `/api/session.research.activeProjectNo` 一致。
- `POST /api/projects`，请求 `{title}`：创建项目和默认会话并选中，返回 `201 {project:Project,conversation:Conversation}`。
- `PATCH /api/projects/{projectNo}`，请求 `{title}`：改名，返回 `200 {project:Project}`；空白、超长或未知字段返回 `400 INVALID_REQUEST`。
- `POST /api/projects/{projectNo}/select`：选择 active 项目，返回 `200 {project:Project,conversation:null|Conversation}`。
- `POST /api/projects/{projectNo}/archive`：执行 R4 fallback，返回 `200 {archivedProject:Project,activeProject:null|Project,activeConversation:null|Conversation}`。
- `POST /api/projects/{projectNo}/restore`：恢复，返回 `200 {project:Project}`；`project.selected` 表明是否因 owner 原指针为空而被选中。

### 5.4 会话 API

- `GET /api/projects/{projectNo}/conversations?status=active|archived&limit=&cursor=`：返回 `{items:Conversation[],nextCursor}`。
- `POST /api/projects/{projectNo}/conversations`，请求 `{title}`：仅 active 项目可创建并选中，返回 `201 {conversation:Conversation}`。
- `PATCH /api/projects/{projectNo}/conversations/{conversationNo}`，请求 `{title}`：返回 `200 {conversation:Conversation}`。
- `POST /api/projects/{projectNo}/conversations/{conversationNo}/select`：同时选择其 active 项目和 active 会话，返回 `200 {project:Project,conversation:Conversation}`。
- `POST /api/projects/{projectNo}/conversations/{conversationNo}/archive`：执行 R5 fallback，返回 `200 {archivedConversation:Conversation,activeConversation:null|Conversation}`。
- `POST /api/projects/{projectNo}/conversations/{conversationNo}/restore`：返回 `200 {conversation:Conversation}`；`conversation.selected` 表明是否因项目原指针为空而被选中。

### 5.5 项目论文与结果 API

- `GET /api/projects/{projectNo}/papers?status=active|removed&limit=&cursor=`：返回 `{items:ProjectPaper[],nextCursor}`，不返回其他会话聊天。
- `PUT /api/projects/{projectNo}/papers/{paperId}`：仅 active 项目可加入或恢复；返回 `200 {paper:ProjectPaper,created:false}`（幂等已有/恢复）或 `201 {paper:ProjectPaper,created:true}`（首次创建）。
- `DELETE /api/projects/{projectNo}/papers/{paperId}`：软移出，返回 `204`；重复移出仍返回 `204`。
- `GET /api/projects/{projectNo}/papers/{paperId}/read-results?limit=&cursor=`：active/removed 均可读取，返回 `{items:ReadResult[],nextCursor}`；每项 content 来自对应 bot message。
- 论文下载沿用稳定论文身份生成当时有效的开放 PDF 入口。只有用户点击才交给浏览器；加入项目、下载和服务端全文缓存是三个独立动作，API 不记录“已下载”。

### 5.6 Web 聊天

1. `POST /api/chat` 沿用 Spec 0002/0003 的 HTTP + SSE、逐帧 JSON、`[DONE]`、工具进度和确定性编排。请求为 `{text}`，响应 `Content-Type: text/event-stream`；服务端从 JWT 或 guest Cookie 推导 actor，再从 active 指针推导项目和会话，聊天请求不得提交 `project_id`、owner ID 或任意目标会话 ID 绕过选择 API。
2. active 项目为空时，聊天入口调用 5.2 的 `ensureActiveProject`；active 项目存在但会话为空时在同一服务事务中懒建该项目默认会话。指针悬空、跨 owner、跨项目或指向 archived 资源时先在事务中清空，再执行相同 fallback/懒创建。
3. SSE 断线不改变精读提交结果；客户端重连后必须先读取 `/api/session`，再按返回的项目/会话编号和资格状态恢复界面。

## 6. Agent 只读能力

1. Router/Executor 继续遵守 ADR 0010：每轮一个业务目标、Go 生成动态最小工具白名单、参数严格校验、单轮单工具结论和既有 trace 规则。
2. 新增工具仅限：
   - `list_current_project_papers(limit,cursor)`：只列当前 active 项目的 active 论文；
   - `get_current_project_paper_read_history(paperId,limit,cursor)`：读取指定 active 项目的精读历史；
   - `answer_from_current_project_papers(paperIds,question)`：仅当用户本轮明确指定论文时使用，`paperIds` 去重后最多 10 个，服务端重新读取摘要或已校验全文形成回答。
3. actor、当前项目由服务端注入；工具 schema 不含 `projectId`、owner ID、SQL、URL 或文件路径。paperId 必须属于当前 active 项目，否则统一返回项目论文不存在。
4. 旧精读内容只能作为待核对历史。`answer_from_current_project_papers` 必须重读证据，缺摘要且无校验全文的论文必须明确不可用于事实结论。
5. 不提供项目/会话管理、论文加入/移出、任意 URL、文件系统、SQL、批量下载或跨项目搜索工具。QQ Agent 不获得这些项目工具。

## 7. 精读短事务

1. 网络抓取、PDF 获取、全文抽取和模型调用必须在数据库事务之外完成。开始前可创建 `delivery_status='reserved'` 的 bot 消息，并按 Spec 0003 原子预留访客资格。
2. 有效输出形成后，在同一个短 MySQL 事务中：
   1. 锁定并重新验证 actor、active 项目、来源会话及论文归属；
   2. 将 bot 消息标记为成功 `sent`；
   3. 创建或恢复 `(project_id,paper_id)` 项目论文；
   4. 创建唯一的 `project_read_results` 关系；
   5. 若 actor 是访客，以相同 `reservation_id` 条件把 `deep_read_state` 从 `reserved` 改为 `consumed`。
3. 上述任一步影响行数或约束不符合预期时全部回滚；不得出现成功消息无结果关系、消费资格无结果、或失败精读自动入项目。账号没有第 5 步。
4. `project_read_results.message_id` 唯一，使同一完成消息的提交可幂等重试；重复提交必须返回已存在结果，不能新增历史或再次消费资格。
5. 浏览器断线但事务已提交仍算成功。纯系统失败且没有可恢复有效结果时，按 Spec 0003 条件退还访客预留；全局全文缓存可保留重试复用。

## 8. 访客导入、升级与清除

### R8. 导入与升级

1. 访客注册新账号时，账号创建和该访客全部项目所有权转移必须在同一 MySQL 事务中完成；项目 ID、会话、消息、项目论文和精读结果关系不复制、不合并。
2. 已有账号登录时仍须明确确认。首次 `POST /api/guest/import` 需要有效账号 JWT、当前 active guest Cookie、`Idempotency-Key` 和 `{confirm:true}`；成功返回 `{importedProjectNos:number[],currentProjectNo:null|number}` 并清 Cookie。
3. 导入端点必须先执行窄化幂等查询，再执行普通 guest 有效性检查：若 token hash 对应 `status='upgraded'`、`upgraded_to_account_id` 等于当前 JWT 账号且幂等键与已完成键完全相同，则允许在 guest 凭证已失效后只重放首次响应并再次清 Cookie；该完成态凭证对所有其他 API、其他账号或不同幂等键仍是无效凭证。
4. 导入时按 `project_no ASC,id ASC` 稳定顺序为冲突项目重新分配目标账号范围的 `project_no`；同名项目保持独立。各项目 `active_conversation_id` 保留；访客原 active 项目若 active，则成为账号当前项目，否则选择导入项目中最近更新的 active 项目；没有 active 项目时不覆盖账号已有有效指针。
5. `guest_sessions.imported_project_ids` 保存首次转移的内部 project ID JSON 数组，`imported_current_project_id` 保存首次选中的内部项目 ID；重放时在同一目标账号 owner 范围重新解析稳定 `project_no`，不得把数据库 ID 暴露给客户端。两个结果字段和幂等键至少保留到该 guest 原 `expires_at`，只用于审计/重放，不作为所有权事实。
6. 相同账号、token hash 和幂等键的重复请求返回首次结果，不重复转移或重新编号；首次请求提交成功但响应丢失也遵守第 3 条。不同幂等键或目标账号返回 `401 INVALID_GUEST_SESSION`，不能读取已完成结果。
7. `deep_read_state='reserved'` 时拒绝首次导入，统一返回 `409 GUEST_DEEP_READ_IN_PROGRESS`。Spec 0003 API 段中的旧名 `GUEST_TASK_IN_PROGRESS` 自本 Spec 起废止，不得用于新响应。成功导入不把未消费资格转成账号额度。
8. 用户拒绝导入时不改变访客项目，到原固定到期时间再清除。导入完成后访客凭证除第 3 条幂等重放外立即失效。

### R9. 清除与到期

1. 主动清除和到期清理先吊销访客凭证，再删除该访客拥有的 projects；通过 FK/显式顺序删除其 conversations、messages、project_papers 和 project_read_results。不得删除全局 `papers`、全文缓存、账号项目或任何 QQ 数据。
2. 清理最终删除或不可恢复化访客资格事实，但不得用于重置资格后继续使用同一凭证。短期 IP 安全限流键不删除。
3. 后台至少每日按 `(expires_at,id)` keyset 分批扫描，任务可分页、可重试、幂等；记录扫描、删除、跳过在途和失败数量，不记录消息正文。
4. `deep_read_state='reserved'` 的到期主体按 Spec 0003 在途策略跳过内容删除或先完成安全回收；不得并发删除正在提交的结果。

## 9. 持久化结构与迁移

1. fresh install 表为 `accounts`、`guest_sessions`、`users`、`projects`、`conversations`、`papers`、`messages`、`project_papers`、`project_read_results`。准确列、索引、CHECK 和 FK 以 `docs/schema.sql` 为准。
2. `projects` owner CHECK、`conversations` 的 `user_id XOR project_id` CHECK、owner/project 范围唯一键和项目子表 FK 必须由 MySQL 强制；active pointer 的同 owner/project 语义由受测事务强制。
3. `0009_research_projects.sql` 是显式手工、破坏性向前迁移。执行前必须创建并验证可恢复备份；MySQL DDL 隐式提交，不能宣称整个迁移可事务回滚。
4. 迁移只删除 `owner_account_id IS NOT NULL OR owner_guest_id IS NOT NULL` 的旧 Web conversations 及对应 messages，并清空旧 Web active conversation 指针。任何 `user_id IS NOT NULL` 的 QQ conversation/message、`users`、`papers`、全文缓存、账号、guest session 及其 `deep_read_state/reservation_id/reserved_at` 必须保留。
5. 迁移不为现有主体批量创建默认项目；新默认结构由应用在首次真实使用时懒创建。旧 Web 导入 ID 已无有效目标，迁移将旧 conversation 语义结果清空并改为 project 语义字段。
6. 删除旧 owner 列前必须动态移除其 FK、旧 CHECK 和索引；新增 project 列、预检 XOR 数据后再建新约束。active 指针不建 FK，避免循环和无法表达归属的伪安全。
7. 应用部署必须采用停写窗口：备份并停止旧 Web 写入，执行 0009，部署只写项目模型的新版本，完成 schema/QQ 数据抽样校验后再恢复流量。不得让旧应用在迁移后运行。

## 10. UI 行为

1. Web 左侧项目切换器展示 active 项目，提供新建、选择、改名、归档及独立归档视图；项目标题重复时同时显示稳定 `projectNo`。归档当前项目后立即渲染服务端返回的 fallback；无 fallback 时显示空态，直到用户创建/恢复项目、明确调用 `ensure-active` 进入工作区，或下一次聊天触发懒创建。
2. 每个项目有独立会话列表。切换项目恢复其 active 会话；没有 active 会话时显示“开始聊天将创建默认会话”。归档最后一个会话不弹出强制创建。
3. 项目资料区以论文卡片展示 active 项目论文、最新精读与历史入口，不显示项目级 `#N`。已移出视图可恢复论文；移出前确认其历史不会删除。
4. 精读历史按新到旧显示，默认展开最新，标明 quick/deep、来源会话、时间和“历史模型输出，新的事实结论将重新核对论文证据”。来源会话归档后仍可查看。
5. 检索/速览卡片提供明确“加入当前项目”；成功精读后卡片自动出现在当前项目。下载按钮只触发浏览器下载，不显示服务端无法确认的“已下载”。
6. 账号和访客 UI 能力相同，仅访客继续展示三十天生命周期和精读资格。登录导入对话框改为“导入访客项目”，列明同名不合并；默认不导入。
7. UI 状态一律以 API 返回为准，不从 Cookie 或本地缓存推断 owner/active 指针。`INVALID_TOKEN` 清理 JWT 但不静默降级；SSE 断线后重新拉取项目、会话和资格状态。

## 11. 错误与故障语义

| 场景 | HTTP / status | 行为 |
|---|---|---|
| actor 无效 | `401 INVALID_TOKEN` 或 `401 INVALID_GUEST_SESSION` | 不降级、不泄露资源 |
| 参数、标题或 status 非法 | `400 INVALID_REQUEST` | 不部分写入 |
| cursor 无效 | `400 INVALID_CURSOR` | 客户端从第一页重取 |
| 项目不存在或不属于 actor | `404 PROJECT_NOT_FOUND` | 跨 owner 同样返回 |
| 项目已归档且动作要求 active | `409 PROJECT_ARCHIVED` | 可先恢复 |
| 会话不存在或不属于项目 | `404 CONVERSATION_NOT_FOUND` | 不泄露跨项目存在性 |
| 会话已归档且动作要求 active | `409 CONVERSATION_ARCHIVED` | 可先恢复 |
| 全局论文不存在 | `404 PAPER_NOT_FOUND` | 不创建关系 |
| 项目论文不存在/不在 active 集合 | `404 PROJECT_PAPER_NOT_FOUND` | Agent 与 API 同样隔离 |
| 标题自然语言匹配不唯一 | `409 PAPER_REFERENCE_AMBIGUOUS` | 返回可澄清卡片，不自动选 |
| 访客精读在途 | `409 GUEST_DEEP_READ_IN_PROGRESS` | 查询任务/结果后重试 |
| 访客资格已消费 | `403 GUEST_DEEP_READ_EXHAUSTED` | 普通项目功能可用 |
| 并发状态已变化 | `409 RESOURCE_STATE_CONFLICT` | 重拉当前状态，不盲重放 |
| MySQL/必要依赖不可用 | `503 DEPENDENCY_UNAVAILABLE` | 短事务不得宣称成功 |
| 安全限流/成本/急停 | 沿用 `RATE_LIMITED` / `COST_BREAKER_OPEN` / `EMERGENCY_STOPPED` | 不与产品资格混用 |

对外不得包含 SQL、数据库主键、其他 owner 信息或内部堆栈。明确可恢复的 429/503 返回 `Retry-After`。项目列表等廉价历史读取不因成本熔断停止，但仍受安全限流。

## 12. 验收标准

实现阶段必须建立单元、真实 MySQL 集成、HTTP/SSE、安全隔离和静态 UI smoke 覆盖；本任务只交付 Spec 与 schema，不创建测试。上线前必须全部满足：

- MySQL 拒绝双 owner/零 owner 项目和同时/均未设置 `user_id`、`project_id` 的会话；Web owner 查询无法读取其他账号、访客、项目或 QQ 会话。
- 新账号/访客首次真实使用懒建默认项目和会话；显式新项目原子创建默认会话；静态访问不制造空数据。
- 项目切换恢复各自当前会话；会话聊天、研究方向、候选、序号和展示记录跨会话隔离，项目上下文只共享项目论文与精读历史。
- 归档当前项目按确定性顺序 fallback；无其他项目时指针为空且下次聊天懒建默认项目。归档最后一个会话允许项目指针为空且下次聊天懒建默认会话。
- 项目/会话恢复、改名保持编号；会话不可跨项目移动；归档/恢复并发不会留下跨 owner 或 archived active 指针。
- 项目论文加入幂等，软移出不删除全局论文、全文、消息、序号或历史，再加入恢复同一关系；项目区不产生第二套 `#N`。
- quick/deep 每次成功结果只引用一条已持久化 bot 消息；历史按新到旧展示。精读短事务任一步失败全部回滚，访客资格、消息、项目论文和结果关系无部分成功。
- Agent 工具 schema 不含 project/owner 任意选择参数，只能读取当前 active 项目；新结论重新读取摘要/校验全文，不把旧精读当唯一证据；QQ 无项目工具。
- 项目、会话、论文和历史列表按本 Spec cursor 合约分页，边界、篡改和并发插入场景无 OFFSET 漂移。
- 注册升级和确认导入转移完整项目树，同名不合并、冲突编号确定性重分配、重复幂等键无副本；在途精读统一返回 `GUEST_DEEP_READ_IN_PROGRESS`。
- 主动清除和到期清理删除访客项目子树并吊销凭证，保留共享 papers/全文、账号项目和全部 QQ 数据；清理可分页重试且不记录正文。
- 在 0008 当前 schema 副本上演练 0009：仅旧 Web conversations/messages 被删，QQ conversations/messages 数量和内容不变，guest `deep_read_state/reservation_id/reserved_at` 逐行不变；新约束、索引和 FK 与 `docs/schema.sql` 一致。
- Spec 0003 未被取代的 Cookie、邮箱验证、JWT、安全限流、成本熔断、人工急停和 SSE 验收全部继续通过。

## 13. 明确排除项

首版不实现：项目分享、协作者、权限角色、公开链接；本地文件夹映射、云端文件工作区、文件上传、项目专属 PDF/全文副本、任意附件、手写笔记或项目级自由文档；QQ 会话加入项目或 Web/QQ 会话合并；跨项目移动/复制会话、自动合并同名项目；Agent 项目管理、论文写操作、任意 URL/文件/SQL、批量下载；自动把全部项目内容注入模型；项目论文第二套序号；账号自助永久删除项目；服务端“已下载”追踪；迁移旧 Web 会话到默认项目；跨浏览器/设备恢复访客项目；分享或跨 owner 搜索。
