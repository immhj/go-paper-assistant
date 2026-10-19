-- ============================================================================
-- 平台层（研究生工具集）· 账号与邀请码   (MySQL 8.0+)
--
--   accounts       平台登录身份（邮箱登录 / 昵称展示 / 用户名归属键）
--   invite_codes   注册准入凭证，可设使用次数与有效期
--
-- 见 ADR 0012（转向工具集平台）、0014（账号身份 + JWT）、0015（邀请码准入）。
-- 平台词汇见 docs/contexts/platform/CONTEXT.md；schema.sql 的 4 张表是论文助手
-- 这一个工具的领域表，与本文件的平台表分属两个上下文，互不改动对方。
--
-- 与论文助手 users 表的关系：accounts.qq_openid 可空绑定到 users.qq_openid，
-- 使同一自然人的 Web 会话与 QQ 会话可合并查看；v1 不强制绑定。
-- ============================================================================

SET NAMES utf8mb4;


-- ============================================================================
-- 1. invite_codes —— 先建，accounts 的 invited_by_code 指向它
-- ============================================================================
-- max_uses = 1 即一次性码；大于 1 即多人共享码。used_count 到 max_uses 即失效。
-- expires_at 为 NULL 表示永不过期。消费必须在事务里用条件自增保证原子：
--
--   UPDATE invite_codes
--      SET used_count = used_count + 1
--    WHERE code = ?
--      AND used_count < max_uses
--      AND (expires_at IS NULL OR expires_at > NOW(3));
--   -- 受影响行数 = 1 才算占用成功；= 0 说明码错/已满/过期，注册应拒绝。
--
-- 使用次数统计的是「成功注册」，校验失败不消费额度（ADR 0015）。
CREATE TABLE invite_codes (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code        VARCHAR(64)     NOT NULL                COMMENT '邀请码明文，注册时输入',
  max_uses    INT UNSIGNED    NOT NULL DEFAULT 1      COMMENT '允许成功注册消费的总次数；1 = 一次性',
  used_count  INT UNSIGNED    NOT NULL DEFAULT 0      COMMENT '已成功消费次数；达到 max_uses 即失效',
  expires_at  DATETIME(3)         NULL                COMMENT '过期时刻；NULL = 永不过期',
  created_by  VARCHAR(64)     NOT NULL                COMMENT '生成者标识（管理员用户名）；邀请码由管理后台生成',
  created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_invite_codes_code (code),
  KEY idx_invite_codes_active (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='注册准入邀请码，可设使用次数与有效期';


-- ============================================================================
-- 2. accounts —— 平台登录身份
-- ============================================================================
-- 身份三字段（ADR 0014）：
--   email     登录凭证，唯一、用户可见、可改。不做邮箱验证码，仅作标识 + 未来找回。
--   nickname  展示名，可改、不唯一，纯装饰，绝不当键。
--   username  系统生成的内部唯一标识，永久稳定、用户不可见，是一切归属的键，写进 JWT。
-- password_hash 用 argon2id，绝不存明文。
-- qq_openid 可空绑定到论文助手 users.qq_openid，合并同一个人两条通道的历史。
-- invited_by_code 记录注册时消费的邀请码，用于追踪准入来源。
CREATE TABLE accounts (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  username         VARCHAR(64)     NOT NULL                COMMENT '系统生成的内部唯一标识，归属键，用户不可见不可改',
  email            VARCHAR(191)    NOT NULL                COMMENT '登录凭证，唯一',
  nickname         VARCHAR(64)     NOT NULL                COMMENT '展示昵称，可改、不唯一',
  password_hash    VARCHAR(255)    NOT NULL                COMMENT 'argon2id 哈希，绝不存明文',
  invited_by_code  VARCHAR(64)         NULL                COMMENT '注册时消费的邀请码，追踪准入来源',
  qq_openid        VARCHAR(64)         NULL                COMMENT '可空绑定到 users.qq_openid；NULL = 未绑定 QQ',
  status           ENUM('active','disabled') NOT NULL DEFAULT 'active' COMMENT '禁用即拒绝登录，用于封禁',
  created_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_accounts_username (username),
  UNIQUE KEY uk_accounts_email (email),
  UNIQUE KEY uk_accounts_qq_openid (qq_openid),
  KEY idx_accounts_invite (invited_by_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='平台账号（邮箱登录 / 昵称展示 / 用户名归属键）';


-- ============================================================================
-- 外键（可选）。单实例、Go 侧统一写入时可先不开；需要数据库级一致性时取消注释。
-- MySQL 唯一键允许多个 NULL，因此 qq_openid 未绑定的多行不冲突。
-- ============================================================================
-- ALTER TABLE accounts ADD CONSTRAINT fk_accounts_invite
--   FOREIGN KEY (invited_by_code) REFERENCES invite_codes(code) ON DELETE SET NULL;
-- ALTER TABLE accounts ADD CONSTRAINT fk_accounts_qq
--   FOREIGN KEY (qq_openid) REFERENCES users(qq_openid) ON DELETE SET NULL;
