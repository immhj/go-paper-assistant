-- ============================================================================
-- 研究生工具集 · 当前权威安装结构 (MySQL 8.0+)
--
-- 本文件是 fresh install 的真实目标 schema。已部署实例必须按 docs/migrations
-- 顺序执行向前迁移；0008 会删除邀请码历史，0009 会删除旧账号/访客 Web 会话，
-- 两者执行前都必须创建可恢复备份。schema-full.sql 仅是历史参考。
--
--   accounts             已验证平台账号与当前项目指针
--   guest_sessions       访客凭证、生命周期、精读资格与项目导入幂等结果
--   users                QQ 匿名用户与当前 QQ 会话指针
--   projects             账号或访客私有研究项目
--   conversations        QQ 用户或研究项目恰一归属的会话
--   papers               跨主体共享论文
--   messages             会话消息、候选与展示记录
--   project_papers       项目对全局论文的可软移出引用
--   project_read_results 项目论文历次精读结果与 bot 消息关系
--
-- 项目四表关系：projects -> conversations，projects -> project_papers
-- -> project_read_results；messages 属于 conversation，结果另引用来源 conversation
-- 和成功 bot message。
-- ============================================================================
-- 不进数据库
--
--   services/paperscraper/data/  固定版本的 CCF 名单与 impact_factor 数据（ADR 0020），
--                         匹配结果冗余写入 papers 的等级列与 rank_version
--   Go 源码内提示词       当前 tracer 的 P2/P3/P4 提示词；稳定后再迁入 config/prompts/*.md
--   环境变量              窗口、限速、模型、邮件和管理后台配置
--   data/fulltext/*.txt   全局论文正文缓存；表中只保存路径和校验状态
--   本地下载状态          浏览器是否最终写入设备，服务端无法可靠确认
-- ============================================================================

SET NAMES utf8mb4;


-- ============================================================================
-- 1. accounts
-- ============================================================================
-- active_project_id 是可空应用指针。简单 FK 只能证明项目存在，不能证明项目属于
-- 此账号，因此有意不建 FK；选择/归档/导入事务必须锁定账号并校验 owner 与 active。
CREATE TABLE accounts (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  username          VARCHAR(64)     NOT NULL COMMENT '稳定内部归属键，不向用户展示',
  email             VARCHAR(191)    NOT NULL COMMENT '统一规范化后的登录邮箱',
  nickname          VARCHAR(64)     NOT NULL,
  password_hash     VARCHAR(255)    NOT NULL COMMENT 'argon2id 哈希，绝不存明文',
  qq_openid         VARCHAR(64)         NULL,
  active_project_id BIGINT UNSIGNED     NULL COMMENT '当前 active 项目；应用事务校验同 owner',
  status            ENUM('active','disabled') NOT NULL DEFAULT 'active',
  auth_version      BIGINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '递增即撤销旧 JWT',
  email_verified_at DATETIME(3)         NULL COMMENT '历史账号可空；新账号必须由应用写入',
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_accounts_username (username),
  UNIQUE KEY uk_accounts_email (email),
  UNIQUE KEY uk_accounts_qq_openid (qq_openid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='平台账号';


-- ============================================================================
-- 2. guest_sessions
-- ============================================================================
-- imported_project_ids/imported_current_project_id 只保存首次导入结果供幂等响应，
-- 项目 owner 才是所有权事实。active_project_id 同样由事务校验，不建循环指针 FK。
CREATE TABLE guest_sessions (
  id                          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  token_hash                  BINARY(32)      NOT NULL COMMENT 'guest_session Cookie 的 SHA-256 摘要',
  status                      ENUM('active','upgraded','cleared','expired') NOT NULL DEFAULT 'active',
  deep_read_state             ENUM('available','reserved','consumed') NOT NULL DEFAULT 'available',
  reservation_id              VARCHAR(191)        NULL,
  reserved_at                 DATETIME(3)         NULL,
  expires_at                  DATETIME(3)     NOT NULL COMMENT '签发时固定 +30 天，不滑动',
  upgraded_to_account_id      BIGINT UNSIGNED     NULL,
  active_project_id           BIGINT UNSIGNED     NULL COMMENT '当前 active 项目；应用事务校验同 owner',
  import_idempotency_key      VARCHAR(191)        NULL,
  imported_project_ids        JSON                NULL COMMENT '首次导入的内部 project ID 数组，仅供幂等重放',
  imported_current_project_id BIGINT UNSIGNED     NULL COMMENT '首次导入选中的项目结果',
  created_at                  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at                  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_guest_sessions_token_hash (token_hash),
  UNIQUE KEY uk_guest_sessions_reservation (reservation_id),
  KEY idx_guest_sessions_cleanup (status, expires_at),
  KEY idx_guest_sessions_account (upgraded_to_account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='30天访客凭证、精读资格与项目导入结果';


-- ============================================================================
-- 3. users
-- ============================================================================
-- 论文助手“用户”仅指 QQ 匿名用户，不是平台账号或访客。
-- active_conversation_id 是可空应用指针；/new、/use、归档 fallback 在事务中校验
-- 会话 user_id。新用户第一次发普通消息时懒创建会话 #1「默认会话」。
CREATE TABLE users (
  id                     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  qq_openid              VARCHAR(64)     NOT NULL COMMENT 'QQ 官方机器人用户标识，不是真实 QQ 号；按 AppID 隔离',
  active_conversation_id BIGINT UNSIGNED     NULL COMMENT '当前 QQ 会话；应用事务校验同 user',
  last_active_at         DATETIME(3)     NOT NULL,
  created_at             DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_users_openid (qq_openid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='QQ 用户及当前会话指针';


-- ============================================================================
-- 4. projects
-- ============================================================================
-- owner_account_id / owner_guest_id 恰一非空。project_no 只在 owner 范围唯一并稳定；
-- 标题可以重复。active_conversation_id 可空且不建循环 FK，应用事务必须验证目标
-- conversation.project_id=id 且 status=active。
CREATE TABLE projects (
  id                     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  owner_account_id       BIGINT UNSIGNED     NULL,
  owner_guest_id         BIGINT UNSIGNED     NULL,
  project_no             INT UNSIGNED    NOT NULL COMMENT 'owner 范围内稳定展示编号',
  title                  VARCHAR(128)    NOT NULL,
  active_conversation_id BIGINT UNSIGNED     NULL COMMENT '当前 active Web 会话；应用事务校验同项目',
  status                 ENUM('active','archived') NOT NULL DEFAULT 'active',
  created_at             DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at             DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_projects_account_no (owner_account_id, project_no),
  UNIQUE KEY uk_projects_guest_no (owner_guest_id, project_no),
  KEY idx_projects_account (owner_account_id, status, updated_at, id),
  KEY idx_projects_guest (owner_guest_id, status, updated_at, id),
  CONSTRAINT ck_projects_single_owner CHECK (
    (owner_account_id IS NOT NULL) + (owner_guest_id IS NOT NULL) = 1
  ),
  CONSTRAINT fk_projects_account FOREIGN KEY (owner_account_id)
    REFERENCES accounts(id) ON DELETE CASCADE,
  CONSTRAINT fk_projects_guest FOREIGN KEY (owner_guest_id)
    REFERENCES guest_sessions(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='账号或访客私有研究项目';


-- ============================================================================
-- 5. conversations
-- ============================================================================
-- QQ 会话只设置 user_id，Web 会话只设置 project_id；CHECK 强制 XOR。
-- conversation_no 在 QQ user 或 project 范围内稳定唯一。Web 会话不能跨项目移动。
--
-- current_context 示例：
-- {
--   "name": "扩散模型做医学影像分割",
--   "spec": {
--     "concepts": [
--       {"concept":"扩散模型","synonyms":["diffusion model","DDPM"]},
--       {"concept":"医学影像分割","synonyms":["medical image segmentation"]}
--     ],
--     "yearFrom": 2021,
--     "exclude": ["survey","review"]
--   },
--   "specVersion": 3,
--   "nextOrdinal": 4,
--   "ordinals": {"101":1,"205":2,"307":3}
-- }
--
-- ordinals 仅属于本会话；同一项目内不同会话也不共享 #N。
CREATE TABLE conversations (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id         BIGINT UNSIGNED     NULL,
  project_id      BIGINT UNSIGNED     NULL,
  conversation_no INT UNSIGNED    NOT NULL COMMENT 'QQ user 或 project 范围内稳定展示编号',
  title           VARCHAR(128)    NOT NULL,
  current_context JSON                NULL COMMENT '本会话研究方向、检索规格和论文序号映射',
  status          ENUM('active','archived') NOT NULL DEFAULT 'active',
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_conversations_no (user_id, conversation_no),
  UNIQUE KEY uk_conversations_project_no (project_id, conversation_no),
  KEY idx_conversations_user (user_id, status, updated_at),
  KEY idx_conversations_project (project_id, status, updated_at, id),
  CONSTRAINT ck_conversations_single_owner CHECK (
    (user_id IS NOT NULL) + (project_id IS NOT NULL) = 1
  ),
  CONSTRAINT fk_conversations_user FOREIGN KEY (user_id)
    REFERENCES users(id) ON DELETE CASCADE,
  CONSTRAINT fk_conversations_project FOREIGN KEY (project_id)
    REFERENCES projects(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='QQ 用户或研究项目恰一归属的会话';


-- ============================================================================
-- 6. papers
-- ============================================================================
-- 论文跨 owner/project/conversation 共享，项目只保存引用。删除项目不得删除论文或全文。
CREATE TABLE papers (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  source           VARCHAR(32)     NOT NULL COMMENT 'arxiv 或 ieee（ADR 0018）',
  external_id      VARCHAR(191)    NOT NULL COMMENT '来源内稳定标识；arXiv 用不带版本号的 ID，IEEE 用小写 DOI',
  title            VARCHAR(1024)   NOT NULL,
  abstract         TEXT                NULL COMMENT '为空时禁止调用模型做速览，直接告知用户缺摘要',
  publication_date DATE                NULL COMMENT '用于计算引用速度：引用数 ÷ 发表至今月数',
  venue_raw        VARCHAR(255)        NULL,
  ccf_rank         ENUM('A','B','C')   NULL COMMENT 'NULL 不代表质量低；只用于展示和加权，绝不硬过滤',
  citation_count   INT UNSIGNED    NOT NULL DEFAULT 0,
  tldr             TEXT                NULL COMMENT '基于摘要生成的可选缓存',
  fulltext_path    VARCHAR(512)        NULL COMMENT '全局 data/fulltext/{id}-{version}.txt',
  fulltext_status  ENUM('not_fetched','ok','bad') NOT NULL DEFAULT 'not_fetched' COMMENT '非 ok 不得作为全文证据；只描述共享开放全文',
  doi              VARCHAR(191)        NULL COMMENT '小写 DOI；IEEE 来源时与 external_id 相同',
  issn             VARCHAR(255)        NULL COMMENT '逗号分隔的 ISSN/eISSN',
  jcr_quartile     ENUM('Q1','Q2','Q3','Q4') NULL COMMENT 'impact_factor 数据按 ISSN 精确匹配',
  cas_quartile     TINYINT UNSIGNED    NULL COMMENT '中科院分区 1-4；第三方整理，无版本与学科',
  impact_factor    DECIMAL(9,3)        NULL,
  rank_version     VARCHAR(128)        NULL COMMENT '发表场所等级所依据的固定数据版本（ADR 0020）',
  PRIMARY KEY (id),
  UNIQUE KEY uk_papers_identity (source, external_id),
  KEY idx_papers_date (publication_date),
  KEY idx_papers_doi (doi)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='跨主体共享的论文本体';


-- ============================================================================
-- 6b. account_paper_fulltexts
-- ============================================================================
-- 账号上传自己合法获取的 PDF 后提取并校验的私有全文（ADR 0019）。只供该账号精读，
-- 绝不写入 papers.fulltext_path；不保存 PDF。账号删除时级联删除。
CREATE TABLE account_paper_fulltexts (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id    BIGINT UNSIGNED NOT NULL,
  paper_id      BIGINT UNSIGNED NOT NULL,
  content       MEDIUMTEXT      NOT NULL COMMENT '账号上传 PDF 提取并校验的私有全文；不保存 PDF',
  content_chars INT UNSIGNED    NOT NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_account_paper_fulltexts (account_id, paper_id),
  KEY idx_account_paper_fulltexts_paper (paper_id),
  CONSTRAINT fk_account_paper_fulltexts_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
  CONSTRAINT fk_account_paper_fulltexts_paper FOREIGN KEY (paper_id) REFERENCES papers(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='账号私有全文，只供上传账号精读';


-- ============================================================================
-- 7. messages
-- ============================================================================
-- 每条消息属于一个 conversation。模型窗口只读取当前会话最近 N 条用户原话；bot
-- 历史默认不回灌，项目精读历史也只能通过受限只读工具按需访问。
-- shown_papers 是本会话候选/展示记录，例如：
-- [{"paperId":205,"ordinal":2,"qualityTier":"venue",
--   "qualityLabel":"CCF A · CVPR 2024","relevance":"high",
--   "relevanceReason":"与该会话检索规格直接相关"}]
CREATE TABLE messages (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  conversation_id BIGINT UNSIGNED NOT NULL,
  role            ENUM('user','bot') NOT NULL,
  qq_msg_id       VARCHAR(128)        NULL COMMENT '用户行=收到 ID；bot 行=被回复目标 ID',
  qq_outbound_id  VARCHAR(128)        NULL COMMENT 'QQ 返回的机器人消息或流 ID',
  msg_seq         INT UNSIGNED    NOT NULL DEFAULT 0 COMMENT '用户行 0；QQ bot 行从 1 递增',
  content         MEDIUMTEXT          NULL COMMENT '用户原话可进窗口；bot 输出默认不回灌',
  shown_papers    JSON                NULL COMMENT '本会话候选论文与稳定 ordinal 展示记录',
  agent_trace     JSON                NULL COMMENT '版本化执行摘要；不得含密钥、完整提示词或隐藏推理',
  delivery_status ENUM('received','reserved','sent','failed') NOT NULL DEFAULT 'received',
  cost_usd        DECIMAL(12,8)   NOT NULL DEFAULT 0,
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_messages_window (conversation_id, role, created_at),
  UNIQUE KEY uk_messages_qq_seq (qq_msg_id, role, msg_seq),
  KEY idx_messages_quota (qq_msg_id, role),
  KEY idx_messages_outbound (qq_outbound_id),
  KEY idx_messages_time (created_at),
  CONSTRAINT fk_messages_conversation FOREIGN KEY (conversation_id)
    REFERENCES conversations(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='会话消息、候选列表与展示记录';


-- ============================================================================
-- 8. project_papers
-- ============================================================================
-- (project_id,paper_id) 永久唯一。移出只写 status=removed；再次加入或成功精读恢复
-- 同一行。项目论文不分配第二套 #N。
CREATE TABLE project_papers (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  project_id BIGINT UNSIGNED NOT NULL,
  paper_id   BIGINT UNSIGNED NOT NULL,
  status     ENUM('active','removed') NOT NULL DEFAULT 'active',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_project_papers_identity (project_id, paper_id),
  KEY idx_project_papers_list (project_id, status, updated_at, id),
  KEY idx_project_papers_paper (paper_id),
  CONSTRAINT fk_project_papers_project FOREIGN KEY (project_id)
    REFERENCES projects(id) ON DELETE CASCADE,
  CONSTRAINT fk_project_papers_paper FOREIGN KEY (paper_id)
    REFERENCES papers(id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='项目对全局论文的可恢复引用';


-- ============================================================================
-- 9. project_read_results
-- ============================================================================
-- 只引用已持久化的成功 bot 消息，不复制正文。message role、消息属于来源会话、
-- 来源会话与项目论文同项目等跨表语义由精读短事务锁定并校验。
CREATE TABLE project_read_results (
  id                     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  project_paper_id       BIGINT UNSIGNED NOT NULL,
  message_id             BIGINT UNSIGNED NOT NULL,
  source_conversation_id BIGINT UNSIGNED NOT NULL,
  read_type              ENUM('quick','deep') NOT NULL,
  created_at             DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_project_read_results_message (message_id),
  KEY idx_project_read_results_history (project_paper_id, created_at, id),
  KEY idx_project_read_results_conversation (source_conversation_id),
  CONSTRAINT fk_project_read_results_project_paper FOREIGN KEY (project_paper_id)
    REFERENCES project_papers(id) ON DELETE CASCADE,
  CONSTRAINT fk_project_read_results_message FOREIGN KEY (message_id)
    REFERENCES messages(id) ON DELETE CASCADE,
  CONSTRAINT fk_project_read_results_conversation FOREIGN KEY (source_conversation_id)
    REFERENCES conversations(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='项目论文历次精读结果与持久化消息关系';


-- ============================================================================
-- Web 项目/会话事务示例（账号；访客将 owner_account_id 换成 owner_guest_id）
-- ============================================================================

-- 第一次真实使用且账号无当前项目：
-- 1) SELECT active_project_id FROM accounts WHERE id=? FOR UPDATE;
-- 2) SELECT COALESCE(MAX(project_no),0)+1 FROM projects
--      WHERE owner_account_id=? FOR UPDATE;
-- 3) INSERT projects(owner_account_id,project_no,title)
--      VALUES (?,?,'默认项目');
-- 4) INSERT conversations(project_id,conversation_no,title,current_context)
--      VALUES (LAST_INSERT_ID(),1,'默认会话',
--        JSON_OBJECT('specVersion',1,'nextOrdinal',1,'ordinals',JSON_OBJECT()));
-- 5) UPDATE projects SET active_conversation_id=? WHERE id=?;
-- 6) UPDATE accounts SET active_project_id=? WHERE id=?;
-- 步骤 1-6 必须同一短事务；显式新项目也使用该事务并自动创建默认会话。

-- 归档当前项目：锁定账号和项目，将项目设 archived；按
--   owner_account_id=? AND status='active' ORDER BY updated_at DESC,id DESC LIMIT 1
-- 选择 fallback 并更新 accounts.active_project_id。没有 fallback 时写 NULL，不立即
-- 创建默认项目；下一次真实项目/聊天操作再懒创建。

-- 归档项目最后一个 active 会话：projects.active_conversation_id 可写 NULL，不立即
-- 创建替代会话；下一次在该 active 项目聊天时分配下一个 conversation_no 并懒建
-- “默认会话”。恢复会话不复用旧编号以外的新编号。

-- 加入/恢复项目论文：
-- INSERT INTO project_papers(project_id,paper_id,status) VALUES (?,?,'active')
-- ON DUPLICATE KEY UPDATE status='active',updated_at=CURRENT_TIMESTAMP(3);
-- 软移出：
-- UPDATE project_papers SET status='removed' WHERE project_id=? AND paper_id=?;

-- 精读完成：外部抓取/模型调用结束后，在一个短事务中将 bot message 标为 sent、
-- 创建/恢复 project_papers、INSERT project_read_results，并仅对访客以 reservation_id
-- 条件消费 deep_read_state。任一步失败全部回滚。


-- ============================================================================
-- QQ 会话命令核心 SQL（与项目完全分离）
-- ============================================================================

-- /new 医学影像分割：在事务中仅按 user_id 分配下一个 conversation_no，INSERT
-- conversations(user_id,...), UPDATE users.active_conversation_id。不得设置 project_id。

-- /list：
-- SELECT conversation_no,title,updated_at FROM conversations
--  WHERE user_id=? AND project_id IS NULL AND status='active'
--  ORDER BY updated_at DESC LIMIT 10;

-- /use 2：
-- SELECT id FROM conversations
--  WHERE user_id=? AND project_id IS NULL AND conversation_no=2 AND status='active';
-- UPDATE users SET active_conversation_id=? WHERE id=?;

-- 自然语言“快速精读第3篇”仍从当前 QQ conversation.current_context.ordinals 解析；
-- QQ 精读不创建 project_papers/project_read_results。


-- ============================================================================
-- 循环 active pointer 说明
-- ============================================================================
-- accounts.active_project_id、guest_sessions.active_project_id、
-- users.active_conversation_id、projects.active_conversation_id 有意不建 FK。普通 FK
-- 不能保证同 owner/project，反而会制造循环和伪安全；所有写指针的事务必须锁定
-- 两端、验证归属与 active 状态。其余所有权边和项目子表关系均由上面的 FK 强制。
