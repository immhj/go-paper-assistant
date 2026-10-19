-- ############################################################################
-- 历史完整参考，不要直接执行。
--
-- 这份文件保留引入应用层多会话**之前**的 4 张表 / 95 字段方案，
-- 其中 users.current_context 的设计已被 conversations.current_context 取代。
-- 当前实际、可执行的 fresh-install 结构以 schema.sql 为唯一权威来源；它包含
-- accounts、guest_sessions，以及 user_id / owner_account_id / owner_guest_id 恰一的
-- conversations。升级实例使用 docs/migrations/0008_free_guest_email_verification.sql。
-- 本文件中的旧邀请码、单 owner 或 users.current_context 概念均不是运行时 schema，
-- 不得据此创建、迁移或校验数据库。
-- ############################################################################

-- ============================================================================
-- 论文助手 · 完整结构 (MySQL 8.0+)   4 张表
--
--   users     用户 + 会话状态 + 研究方向（含检索规格与序号映射）
--   papers    论文元数据 + tldr + 全文状态（正文走文件系统，不进库）
--   messages  收发消息一张表 + 候选列表 / 展示记录 + token 成本
--   tasks     异步任务与步骤
--
-- 术语与 CONTEXT.md 对齐：论文 / 版本 / 全文 / 研究方向 / 检索规格 / 领域标签 /
--                        候选列表 / 序号 / 展示记录 / 指代 / 速览 / 精读 /
--                        相关性 / 质量 / 质量信号 / 会话 / 活跃方向 / 任务
--
-- ============================================================================
-- 不进数据库的东西（这是从 19 张压到 4 张的主要来源）
-- ============================================================================
--
--   config/ccf-2026.json          CCF 推荐列表约 600 行，一年更新一次、从不写入。
--                                 启动时读进内存建别名索引；匹配结果冗余进 papers.ccf_rank
--   config/prompts/*.md           提示词 P1–P6 各版本一个文件。
--                                 放文件比放表**更好**：能进 git，版本历史干净，
--                                 热更新 = 重读文件。文件名即版本号，写进 messages 的成本明细里
--   config/prompts/active.yaml    哪个版本当前生效
--   config/settings.yaml          阈值与开关（原 system_settings 的 11 条）：
--                                   width.tooBroad 20000        命中超过此值拒绝执行并追问
--                                   width.tooNarrow 10          低于此值建议放宽
--                                   quality.minCohortSize 30    候选集小于此值不显示百分位
--                                   quality.tooNewDays 60       不足此天数判为过新、无引用信号
--                                   compare.maxPapers 3         多篇对比上限
--                                   session.windowSize 10       用户原话滑动窗口条数
--                                   deliver.abandonAfterMin 55  超时放弃推送，转下次发言时告知
--                                   ratelimit.s2Rps 1
--                                   ratelimit.arxivIntervalMs 3000
--                                   source.hfPapersEnabled true
--                                   source.openalexEnabled false
--   ADMIN_USER / ADMIN_PASSWORD_HASH   单管理员，环境变量。argon2id 哈希，绝不存明文
--   data/fulltext/{paperId}-{version}.txt   论文正文。大文本放文件系统，
--                                 表里只留路径与校验状态，避免 LONGTEXT 拖慢 papers 的常规查询
--
-- ============================================================================
-- 相比 19 张版本，明确放弃的四件事
-- ============================================================================
--
--   1. 序号唯一性从 UNIQUE(direction_id, ordinal) 降级为代码保证。
--      前提是单实例、单写入者（ADR 0006），序号分配不存在竞争。
--   2. 精读不跨用户复用（原 analyses 表）。同一篇被两个人精读要付两次钱，
--      约 $0.009/次；不同人读同一篇的概率不高，可接受。
--   3. 限速排队与 429 统计（原 data_source_calls）只进日志，后台看不到 wait_ms。
--   4. 后台会话不可服务端吊销（改用短时效签名 cookie）。单管理员场景影响很小。
--
--   仍然保留的三条硬约束：
--   1. 任何模型产出都带 prompt 版本与模型名，否则改 prompt 后会静默返回旧结果
--   2. 全文必须先过校验才送入模型 → papers.fulltext_status（防幻觉的主闸门）
--   3. 任务按步骤记录产出，重试不重跑已经花过钱的模型调用 → tasks.steps
--
-- CREATE 语句不含内联外键，可从上到下直接执行。外键在文件末尾用 ALTER 追加，默认注释掉。
-- ============================================================================

SET NAMES utf8mb4;


-- ============================================================================
-- 1. users —— 用户 + 会话 + 研究方向
-- ============================================================================
--
-- 会话与用户严格 1:1：QQ 单聊只有一条永久消息流，用户无法新建或关闭会话。
-- 会话字段只存标识符与指针，**不存任何模型输出**（ADR 0004 三层记忆）。
-- openid 是 per-AppID 的，官方平台不提供 unionid，同一自然人在别的机器人下是另一个身份。
--
-- directions 的形状（键是方向名，用户用它切换：/方向 医学分割）：
-- {
--   "扩散模型做医学影像分割": {
--     "rawUtterance": "帮我找用扩散模型做医学影像分割的高引论文",
--     "spec": {
--       "concepts": [
--         {"concept":"扩散模型","synonyms":["diffusion model","denoising diffusion","DDPM"],
--          "enabled":true,"origin":"compiled"},
--         {"concept":"分割","synonyms":["segmentation"],"enabled":true,"origin":"compiled"},
--         {"concept":"医学影像","synonyms":["medical image","MRI","CT"],"enabled":true,"origin":"user"}
--       ],
--       "yearFrom": 2021, "yearTo": null,
--       "exclude": ["survey","review"],
--       "minCitation": null
--     },
--     "specVersion": 3,
--     "compiledModel": "deepseek-flash", "compiledPromptVersion": "v5",
--     "compiledAt": "2026-09-14T21:40:11+08:00",
--     "nextOrdinal": 24,
--     "ordinals": {"1041":1,"1042":2,"1188":7},
--     "termSuggestions": [{"term":"tool calling","hitCount":23,"concept":"工具调用"}],
--     "lastTotalHits": 342, "searchCount": 5,
--     "lastSearchAt": "2026-09-14T22:08:15+08:00", "archived": false
--   }
-- }
--
-- 概念之间取交集，同义词之间取并集。研究方向不是分类标签 ——
-- "计算机视觉"、"扩散模型做医学影像分割"、"LLM agent 的工具调用" 是同一类对象的三个实例，
-- 没有层级关系；交叉领域只是概念多了一个。
--
-- ordinals 是**序号的唯一登记处**：论文首次在该方向被展示时分配，此后永不改变，
-- 第二次检索再命中它序号不变，因此 "#3" 在方向的生命周期内永久唯一。
-- nextOrdinal 单调递增、永不重置。分配时必须与 nextOrdinal 自增在同一次
-- UPDATE ... WHERE id=? 里完成（单实例单写入者，无竞争）。
-- minCitation 通常留空 —— 绝对阈值在新兴方向会直接筛空，改用同龄组相对排名。
CREATE TABLE users (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  qq_openid         VARCHAR(64)     NOT NULL                COMMENT '单聊 user_openid，业务唯一键',
  nickname          VARCHAR(64)         NULL,
  proactive_allowed TINYINT(1)      NOT NULL DEFAULT 1      COMMENT '由 C2C_MSG_REJECT / C2C_MSG_RECEIVE 事件维护；关闭后主动消息一律失败',
  rate_limited      TINYINT(1)      NOT NULL DEFAULT 0      COMMENT '后台限流开关，命中时拒绝新任务',

  -- 会话状态
  active_direction  VARCHAR(128)        NULL                COMMENT 'directions 里的键；NULL = 尚未建立或已被 /新建 清空',
  focused_paper_id  BIGINT UNSIGNED     NULL                COMMENT '最近聚焦的单篇论文，用于解析"上面这篇"',
  window_started_at DATETIME(3)         NULL                COMMENT '上次切换/新建方向的时刻；用户原话滑动窗口从此刻起算',

  directions        JSON                NULL                COMMENT '见上方形状说明：方向名 → {检索规格, 序号映射, 术语建议, 统计}',

  first_seen_at     DATETIME(3)     NOT NULL,
  last_active_at    DATETIME(3)     NOT NULL,
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_users_openid (qq_openid),
  KEY idx_users_active (last_active_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户 + 会话状态 + 研究方向';


-- ============================================================================
-- 2. papers —— 论文本体
-- ============================================================================
--
-- 一项研究工作，独立于它发在哪里。只增不删，跨用户共享 ——
-- 这是 Semantic Scholar 只有 1 RPS 之下唯一的出路，不是"顺便存一下"。
-- 外部标识用列而不是 (source, external_id) 子表：来源是已知的少数几个，不是开放集合。
-- CCF 等级冗余在这里而不是 join venues 表：等级表走 config/ccf-2026.json，
-- 匹配上就把结果写进来；换版本时批量回写（一年一次）。
CREATE TABLE papers (
  id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  s2_corpus_id       BIGINT UNSIGNED     NULL            COMMENT '身份锚点：S2 已合并预印本与正式版，正合"论文"定义',
  doi                VARCHAR(128)        NULL,
  arxiv_id           VARCHAR(32)         NULL            COMMENT '不带版本号，如 2301.12345',
  openalex_id        VARCHAR(32)         NULL,
  title              VARCHAR(1024)   NOT NULL,
  abstract           TEXT                NULL            COMMENT '为空时**禁止调用模型做速览**，必须直接告知用户缺摘要',
  authors            JSON                NULL            COMMENT '[{"name":"Jonathan Ho","s2Id":"1234567"}]，按署名顺序',
  publication_year   SMALLINT UNSIGNED   NULL,
  publication_date   DATE                NULL            COMMENT '精确到月才能算引用速度；缺失时退化到年',

  -- 发表场所与等级。等级只用于加权与展示，**绝不硬过滤** ——
  -- ECCV 是 CCF B、CVPR/ICCV 是 A，"只要 A"会把 CV 三大会之一整个丢掉
  venue_raw          VARCHAR(255)        NULL            COMMENT 'S2 返回的原始场所名',
  venue_canonical    VARCHAR(255)        NULL            COMMENT '经别名归并后的规范名（NIPS → NeurIPS）',
  ccf_rank           ENUM('A','B','C')   NULL            COMMENT 'NULL = 未被 CCF 收录，不等于质量低',
  ccf_category       VARCHAR(64)         NULL            COMMENT '如 人工智能 / 交叉、综合、新兴',

  is_open_access     TINYINT(1)      NOT NULL DEFAULT 0,
  open_access_pdf_url VARCHAR(1024)      NULL,

  -- 领域标签：入库后从 arXiv 分类与 OpenAlex 主题**派生**，仅用于后台统计，不作为用户选方向的手段。
  -- CV+医学的论文典型是 eess.IV 主分类 + cs.CV 交叉列表 —— 交叉性已写在数据里，无需分类器
  arxiv_primary      VARCHAR(24)         NULL            COMMENT '如 eess.IV',
  arxiv_categories   JSON                NULL            COMMENT '含交叉列表 ["eess.IV","cs.CV"]',
  openalex_topic     VARCHAR(255)        NULL            COMMENT '如 Medical Image Segmentation',

  -- 质量的**原始数据**（结论在 messages.shown_papers）。慢变量，按 TTL 刷新（7–30 天）
  citation_count     INT UNSIGNED    NOT NULL DEFAULT 0,
  influential_citation_count INT UNSIGNED NOT NULL DEFAULT 0,
  reference_count    INT UNSIGNED    NOT NULL DEFAULT 0,
  hf_upvotes         INT UNSIGNED        NULL            COMMENT 'HF Daily Papers 热度。过新档位的唯一外部信号，属社区关注度而非学术质量',
  metrics_fetched_at DATETIME(3)         NULL            COMMENT 'TTL 起点；过期只影响新鲜度，不影响可用性',

  -- "这篇讲什么"：与方向无关，是论文自身属性，**全局共享、跨用户跨方向复用**。
  -- 与之相对，"这篇是否符合你的方向"必须按方向计算，存在 messages.shown_papers 里
  tldr               TEXT                NULL,
  tldr_source        ENUM('s2','generated') NULL,
  tldr_prompt_version VARCHAR(32)        NULL            COMMENT 'generated 时必填，否则改 prompt 后静默返回旧结果',

  -- 版本 + 全文。只为"打算读的那一版"取全文，所以一行装得下。
  -- 正文落 data/fulltext/{id}-{version}.txt，不进数据库
  fulltext_version   VARCHAR(64)         NULL            COMMENT '如 v3 / CVPR 2023',
  fulltext_path      VARCHAR(512)        NULL,
  fulltext_chars     INT UNSIGNED    NOT NULL DEFAULT 0,
  fulltext_tokens    INT UNSIGNED    NOT NULL DEFAULT 0  COMMENT '估算值，用于预判上下文预算',
  -- **防幻觉的主闸门**：非 ok 一律不送入模型。
  -- 否则模型会基于乱码产出格式完美、内容虚构的精读，而这种错误极难发现
  fulltext_status    ENUM('not_fetched','ok','too_short','no_structure','unreadable','extract_failed') NOT NULL DEFAULT 'not_fetched' COMMENT '防幻觉主闸门：非 ok 一律不送入模型',
  fulltext_note      VARCHAR(512)        NULL,
  fulltext_fetched_at DATETIME(3)        NULL,

  created_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_papers_corpus (s2_corpus_id),
  UNIQUE KEY uk_papers_doi (doi),
  UNIQUE KEY uk_papers_arxiv (arxiv_id),
  KEY idx_papers_date (publication_date),
  KEY idx_papers_ccf (ccf_rank),
  KEY idx_papers_ttl (metrics_fetched_at),
  KEY idx_papers_fulltext (fulltext_status),
  FULLTEXT KEY ft_papers_title (title)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='论文本体（只增不删，跨用户共享的资产）';


-- ============================================================================
-- 3. messages —— 收发消息 + 候选列表 + 成本
-- ============================================================================
--
-- 收发一张表，用 role 区分。代价是一半列对某一方永远是 NULL；
-- 换来的是"对话"这件事只有一处需要看。
--
-- 这张表同时承担三个角色，因为它们本来就是同一件事的三个侧面：
--   1. 用户原话的全量日志，也是**原话滑动窗口**的唯一来源（ADR 0004 第二层）：
--      WHERE user_id=? AND direction=? AND role='user' AND created_at > users.window_started_at
--      ORDER BY created_at DESC LIMIT 10
--   2. **展示记录**：机器人发出候选列表时填 shown_papers，这是解析"上面这篇"、
--      "那篇 diffusion 的"、以及引用回复的唯一依据。存的是系统自身的展示行为，不是模型输出
--   3. **候选列表快照**：持久保留、不可重排 —— 用户三天后回来说"精读 #3"必须仍然精确
--
-- shown_papers 的形状（相关性与质量**分列存，不合成单一推荐分** ——
-- 合成会同时损失两种信息：高质量可以完全不相关，低引用的新论文可以高度相关）：
-- [
--   {"paperId":1188,"ordinal":7,"position":0,
--    "qualityTier":"venue","qualityLabel":"CCF B · MICCAI 2023",
--    "citationVelocity":12.1,"velocityPercentile":null,"cohortSize":null,
--    "relevance":"high","relevanceReason":"正是扩散模型做医学影像分割，且给出了…"},
--   {"paperId":1204,"ordinal":8,"position":1,
--    "qualityTier":"too_new","qualityLabel":"过新（3 周）· 暂无引用数据 · HF 142 赞",
--    "relevance":"medium","relevanceReason":"方法相关但用的是超声而非 MRI"}
-- ]
-- qualityTier 三档互斥：venue / citation_velocity / too_new。
-- "过新"是**合法结论**，填成 0 会被读成"没人认可"，那是主动误导。
-- 候选集少于 30 篇时 velocityPercentile 留空（百分位不稳定）。
-- relevance 在 abstract 为空时必须为 null 并在 reason 说明信息不足，不得据标题推测。
--
-- cost_breakdown 的形状（一条机器人回复可能对应多次模型调用，
-- 例如一次检索 = P1 意图分流 + P2 编译规格 + P3 批量相关性）：
-- [
--   {"promptKey":"P2_compile","promptVersion":"v5","model":"deepseek-flash","thinking":false,
--    "in":1240,"cached":880,"out":310,"reasoning":0,"peak":false,"costUsd":0.00031,"ms":1120},
--   {"promptKey":"P3_relevance","promptVersion":"v3","model":"deepseek-flash","thinking":false,
--    "in":11800,"cached":2600,"out":820,"reasoning":0,"peak":false,"costUsd":0.00186,"ms":3410}
-- ]
-- cached 单列是因为命中上下文缓存的输入单价约为未命中的 1/50 —— 后台要能算出缓存省了多少钱。
-- peak 单列是因为峰时单价翻倍（UTC 周一至周五 01–04、06–10 点）。
-- reasoning 单列是因为思考模式的推理 token 也计费，而 P1–P3 关思考、P4/P5 开。
CREATE TABLE messages (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id           BIGINT UNSIGNED NOT NULL,
  direction         VARCHAR(128)        NULL            COMMENT '当时的活跃方向名；原话窗口按此过滤',
  role              ENUM('user','bot') NOT NULL,
  task_id           BIGINT UNSIGNED     NULL,

  -- ↓ role='user'
  qq_msg_id         VARCHAR(128)        NULL            COMMENT '被动回复凭证。单聊有效期 60 分钟、可回复 4 次',
  qq_event_id       VARCHAR(128)        NULL,
  quoted_message_id BIGINT UNSIGNED     NULL            COMMENT '引用回复了哪条机器人消息（message_type=103）',
  resolved_intent   VARCHAR(32)         NULL            COMMENT 'search / refine / read / switch / new / list / help / unknown',
  resolved_by       ENUM('rule','llm')  NULL            COMMENT 'rule = 规则命中，微秒级且不会错；llm = 走了 P1',
  window_expires_at DATETIME(3)         NULL            COMMENT 'qq_msg_id 收到时刻 + 60min，超过只能发主动消息',
  reply_count       TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '这条用户消息已用掉的被动回复次数，上限 4。一整个流式消息只算 1 次',

  -- ↓ role='bot'
  reply_to_id       BIGINT UNSIGNED     NULL            COMMENT '被动回复的目标用户消息；NULL = 主动消息',
  send_mode         ENUM('passive','proactive','wakeup') NULL,
  msg_type          TINYINT UNSIGNED    NULL            COMMENT '0 文本 / 2 markdown / 7 富媒体',
  msg_seq           INT UNSIGNED        NULL            COMMENT '同一 qq_msg_id 下递增，用于去重',
  stream_msg_id     VARCHAR(64)         NULL            COMMENT '非空即流式。首片由服务端返回，后续分片携带',
  chunk_count       SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  remain_msg_len    INT                 NULL            COMMENT '末次响应返回的剩余字符数，接近 0 需另开新流',
  send_status       ENUM('sending','sent','failed') NULL,
  error_code        VARCHAR(32)         NULL            COMMENT '如 40054013 用户拒收 / 40034128 被动回复超限',

  content           MEDIUMTEXT          NULL            COMMENT '用户侧是原话（一手数据，可进模型上下文）；机器人侧是最终全文（仅供复盘，不进模型输入）',

  -- 候选列表 / 展示记录（role='bot' 且发的是列表时）
  shown_papers      JSON                NULL            COMMENT '见上方形状说明：序号 + 质量档位 + 相关性判断',
  total_hits        INT UNSIGNED        NULL            COMMENT '宽度检测拿到的命中总数。"从 342 篇里筛出 10 篇"的依据',
  width_verdict     ENUM('ok','too_broad','too_narrow') NULL,
  spec_version      INT UNSIGNED        NULL            COMMENT '产生这份列表时的规格版本；与 prompt 版本一起决定判断能否复用',

  -- 成本
  cost_usd          DECIMAL(12,8)   NOT NULL DEFAULT 0,
  cost_breakdown    JSON                NULL            COMMENT '见上方形状说明：每次模型调用的 promptKey / 版本 / token / 缓存 / 峰谷 / 金额',

  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_msg_qq (qq_msg_id),
  KEY idx_msg_window (user_id, direction, role, created_at),
  KEY idx_msg_time (created_at),
  KEY idx_msg_reply (reply_to_id),
  KEY idx_msg_task (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='收发消息 + 候选列表 + 展示记录 + 成本';


-- ============================================================================
-- 4. tasks —— 异步任务
-- ============================================================================
--
-- 留这张表不是为了后台好看，是因为任务本身是异步的：流式推送、55 分钟兜底、
-- 超窗转主动消息。只存在内存里的话，进程一重启就丢，而用户还在等。
--
-- 步骤内联为 JSON：永远整份读出来展示，不会跨任务聚合步骤。
-- 步骤级持久化的真正理由是**省钱** ——
-- S2 返回 429 重试时，不能把前面已经花掉的编译调用重跑一遍。
--
-- steps 形状（search 的步骤序列）：
-- [
--   {"seq":0,"name":"compile_spec","exec":"llm","status":"succeeded","ms":1120,"output":{"specVersion":3}},
--   {"seq":1,"name":"width_check","exec":"code","status":"succeeded","ms":910,"output":{"total":342}},
--   {"seq":2,"name":"search","exec":"datasource","status":"failed","ms":1070,"error":"429"},
--   {"seq":3,"name":"enrich","exec":"code","status":"pending"},
--   {"seq":4,"name":"quality_tier","exec":"code","status":"pending"},
--   {"seq":5,"name":"relevance","exec":"llm","status":"pending"},
--   {"seq":6,"name":"render","exec":"code","status":"pending"}
-- ]
-- exec 的分布本身是个说明：一半以上的步骤是 code，不经过模型。
-- 精读的步骤序列：resolve_reference(llm) → echo_title(code) → fetch_fulltext(datasource)
--                → validate_fulltext(code，防幻觉闸门) → deep_read(llm)
CREATE TABLE tasks (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  kind              ENUM('search','deep_read','compare','term_suggest','subscription_fetch') NOT NULL,
  user_id           BIGINT UNSIGNED     NULL            COMMENT 'NULL = 系统批量任务',
  direction         VARCHAR(128)        NULL,
  trigger_message_id BIGINT UNSIGNED    NULL            COMMENT '触发它的用户消息，也是被动回复凭证的来源',
  priority          TINYINT UNSIGNED NOT NULL DEFAULT 10 COMMENT '小者优先。交互式 10，后台批量 100 —— 否则夜间批处理会把白天用户压到几百个请求之后',
  status            ENUM('queued','running','succeeded','failed','cancelled') NOT NULL DEFAULT 'queued',
  current_step      VARCHAR(64)         NULL,
  steps             JSON                NULL            COMMENT '见上方形状说明。已完成步骤的 output 使重试从断点继续，不重跑已花钱的调用',
  attempt           TINYINT UNSIGNED NOT NULL DEFAULT 0,
  error_code        VARCHAR(64)         NULL,
  error_message     VARCHAR(1024)       NULL,
  deliver_state     ENUM('pending','delivered','deferred','rejected') NOT NULL DEFAULT 'pending' COMMENT 'deferred = 超过约 55 分钟放弃推送，结果存库等用户下次发言时告知',
  total_cost_usd    DECIMAL(12,8)   NOT NULL DEFAULT 0,
  queued_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  started_at        DATETIME(3)         NULL,
  finished_at       DATETIME(3)         NULL,
  PRIMARY KEY (id),
  KEY idx_tasks_queue (status, priority, queued_at),
  KEY idx_tasks_user (user_id, queued_at),
  KEY idx_tasks_kind (kind, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='异步任务（步骤内联为 JSON）';


-- ============================================================================
-- 后台的几个统计怎么查（表少了之后靠 JSON 函数，在这个量级完全够）
-- ============================================================================
--
-- 每日任务数与成本（概览的两张折线图）
--   SELECT DATE(created_at) d,
--          SUM(task_id IN (SELECT id FROM tasks WHERE kind='search'))    search_cnt,
--          SUM(task_id IN (SELECT id FROM tasks WHERE kind='deep_read')) read_cnt,
--          SUM(cost_usd) cost
--     FROM messages WHERE role='bot' AND created_at >= ? GROUP BY d;
--
-- 成本按提示词切分（设置页的提示词表）
--   SELECT j.promptKey, j.promptVersion, COUNT(*) calls,
--          SUM(j.`in`) in_tok, SUM(j.cached) cached_tok, SUM(j.out) out_tok, SUM(j.costUsd) cost
--     FROM messages m,
--          JSON_TABLE(m.cost_breakdown, '$[*]' COLUMNS (
--            promptKey VARCHAR(32) PATH '$.promptKey',
--            promptVersion VARCHAR(32) PATH '$.promptVersion',
--            `in` INT PATH '$.in', cached INT PATH '$.cached', out INT PATH '$.out',
--            costUsd DECIMAL(12,8) PATH '$.costUsd')) j
--    WHERE m.created_at >= ? GROUP BY j.promptKey, j.promptVersion;
--
-- 解析"上面这篇"（取最近一条带候选的机器人消息；若只含 1 篇即可确定，多篇则必须反问）
--   SELECT id, shown_papers FROM messages
--    WHERE user_id=? AND direction=? AND role='bot' AND shown_papers IS NOT NULL
--    ORDER BY created_at DESC LIMIT 1;
--
-- 解析引用回复
--   SELECT shown_papers FROM messages WHERE id = (SELECT quoted_message_id FROM messages WHERE id=?);
--
-- 复用同规格同 prompt 的旧相关性判断（原 relevance_judgements 表的替代）
--   SELECT j.relevance, j.relevanceReason FROM messages m,
--          JSON_TABLE(m.shown_papers, '$[*]' COLUMNS (
--            paperId BIGINT PATH '$.paperId',
--            relevance VARCHAR(8) PATH '$.relevance',
--            relevanceReason VARCHAR(512) PATH '$.relevanceReason')) j
--    WHERE m.user_id=? AND m.direction=? AND m.spec_version=? AND j.paperId=?
--    ORDER BY m.created_at DESC LIMIT 1;
--
-- 论文被展示过几次（原 displayed_papers 的索引换成这个；量级不大，全表扫可接受）
--   SELECT COUNT(*) FROM messages m,
--          JSON_TABLE(m.shown_papers,'$[*]' COLUMNS (paperId BIGINT PATH '$.paperId')) j
--    WHERE j.paperId = ?;
--
-- 若某个统计真的慢了，用 MySQL 8 的生成列 + 索引把热字段提出来，
-- 不要急着重新拆表：
--   ALTER TABLE messages ADD COLUMN shown_count SMALLINT
--     GENERATED ALWAYS AS (JSON_LENGTH(shown_papers)) STORED, ADD KEY (shown_count);
--
-- ============================================================================
-- 外键（可选）。单实例、Go 侧统一写入，也可以只靠应用层保证。要开就逐条执行：
-- ============================================================================
-- ALTER TABLE messages ADD CONSTRAINT fk_msg_user  FOREIGN KEY (user_id)            REFERENCES users(id) ON DELETE CASCADE;
-- ALTER TABLE messages ADD CONSTRAINT fk_msg_quote FOREIGN KEY (quoted_message_id)  REFERENCES messages(id) ON DELETE SET NULL;
-- ALTER TABLE messages ADD CONSTRAINT fk_msg_reply FOREIGN KEY (reply_to_id)        REFERENCES messages(id) ON DELETE SET NULL;
-- ALTER TABLE messages ADD CONSTRAINT fk_msg_task  FOREIGN KEY (task_id)            REFERENCES tasks(id) ON DELETE SET NULL;
-- ALTER TABLE tasks    ADD CONSTRAINT fk_task_user FOREIGN KEY (user_id)            REFERENCES users(id) ON DELETE CASCADE;
-- ALTER TABLE tasks    ADD CONSTRAINT fk_task_msg  FOREIGN KEY (trigger_message_id) REFERENCES messages(id) ON DELETE SET NULL;
