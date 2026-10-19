-- ============================================================================
-- 免费访客、邮箱验证与三类会话归属 (MySQL 8.0+)
-- ADR 0016 / Spec 0003
--
-- 破坏性提醒：本迁移永久删除 invite_codes 以及 accounts.invited_by_code。
-- 如需保留历史准入信息，必须在执行前单独备份。迁移不会把历史账号自动标记为
-- 已验证；新增 email_verified_at 保持 NULL，只有完成验证的新流程才能写入时间。
--
-- 每个 ALTER/INDEX/DROP 都先查询 information_schema，可在部分执行或完成后重跑。
-- ============================================================================

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS accounts (
  id                     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  username               VARCHAR(64)     NOT NULL,
  email                  VARCHAR(191)    NOT NULL,
  nickname               VARCHAR(64)     NOT NULL,
  password_hash          VARCHAR(255)    NOT NULL,
  qq_openid              VARCHAR(64)         NULL,
  status                 ENUM('active','disabled') NOT NULL DEFAULT 'active',
  auth_version           BIGINT UNSIGNED NOT NULL DEFAULT 1,
  email_verified_at      DATETIME(3)         NULL,
  active_conversation_id BIGINT UNSIGNED     NULL,
  created_at             DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at             DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_accounts_username (username),
  UNIQUE KEY uk_accounts_email (email),
  UNIQUE KEY uk_accounts_qq_openid (qq_openid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='平台账号';

CREATE TABLE IF NOT EXISTS guest_sessions (
  id                               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  token_hash                       BINARY(32)      NOT NULL COMMENT '不透明 Cookie 的 SHA-256 摘要，绝不保存明文',
  status                           ENUM('active','upgraded','cleared','expired') NOT NULL DEFAULT 'active',
  deep_read_state                  ENUM('available','reserved','consumed') NOT NULL DEFAULT 'available',
  reservation_id                   VARCHAR(191)        NULL,
  reserved_at                      DATETIME(3)         NULL,
  expires_at                       DATETIME(3)     NOT NULL COMMENT '签发时固定为 +30 天，不滑动续期',
  upgraded_to_account_id           BIGINT UNSIGNED     NULL,
  active_conversation_id           BIGINT UNSIGNED     NULL,
  import_idempotency_key           VARCHAR(191)        NULL,
  imported_conversation_ids        JSON                NULL,
  imported_current_conversation_id BIGINT UNSIGNED     NULL,
  created_at                       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at                       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_guest_sessions_token_hash (token_hash),
  UNIQUE KEY uk_guest_sessions_reservation (reservation_id),
  KEY idx_guest_sessions_cleanup (status, expires_at),
  KEY idx_guest_sessions_account (upgraded_to_account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='30天访客凭证、精读资格与导入结果';

DROP PROCEDURE IF EXISTS migrate_0008;
DELIMITER $$
CREATE PROCEDURE migrate_0008()
BEGIN
  DECLARE object_exists INT DEFAULT 0;
  DECLARE invalid_owners BIGINT DEFAULT 0;
  DECLARE invite_fk_name VARCHAR(64);

  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='accounts' AND column_name='auth_version';
  IF object_exists = 0 THEN
    ALTER TABLE accounts ADD COLUMN auth_version BIGINT UNSIGNED NOT NULL DEFAULT 1 AFTER status;
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='accounts' AND column_name='email_verified_at';
  IF object_exists = 0 THEN
    ALTER TABLE accounts ADD COLUMN email_verified_at DATETIME(3) NULL AFTER password_hash;
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='accounts' AND column_name='active_conversation_id';
  IF object_exists = 0 THEN
    ALTER TABLE accounts ADD COLUMN active_conversation_id BIGINT UNSIGNED NULL AFTER qq_openid;
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='conversations' AND column_name='owner_account_id';
  IF object_exists = 0 THEN
    ALTER TABLE conversations ADD COLUMN owner_account_id BIGINT UNSIGNED NULL AFTER user_id;
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='conversations' AND column_name='owner_guest_id';
  IF object_exists = 0 THEN
    ALTER TABLE conversations ADD COLUMN owner_guest_id BIGINT UNSIGNED NULL AFTER owner_account_id;
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='conversations' AND column_name='user_id' AND is_nullable='YES';
  IF object_exists = 0 THEN
    ALTER TABLE conversations MODIFY COLUMN user_id BIGINT UNSIGNED NULL COMMENT 'QQ owner; exactly one owner column is set';
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='conversations' AND index_name='idx_conversations_user';
  IF object_exists = 0 THEN
    CREATE INDEX idx_conversations_user ON conversations (user_id, status, updated_at);
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='conversations' AND index_name='idx_conversations_account';
  IF object_exists = 0 THEN
    CREATE INDEX idx_conversations_account ON conversations (owner_account_id, status, updated_at);
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='conversations' AND index_name='idx_conversations_guest';
  IF object_exists = 0 THEN
    CREATE INDEX idx_conversations_guest ON conversations (owner_guest_id, status, updated_at);
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='conversations' AND index_name='uk_conversations_account_no';
  IF object_exists = 0 THEN
    CREATE UNIQUE INDEX uk_conversations_account_no ON conversations (owner_account_id, conversation_no);
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='conversations' AND index_name='uk_conversations_guest_no';
  IF object_exists = 0 THEN
    CREATE UNIQUE INDEX uk_conversations_guest_no ON conversations (owner_guest_id, conversation_no);
  END IF;

  SELECT COUNT(*) INTO invalid_owners FROM conversations
   WHERE (user_id IS NOT NULL) + (owner_account_id IS NOT NULL) + (owner_guest_id IS NOT NULL) <> 1;
  IF invalid_owners <> 0 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='0008 refused: conversations contains rows without exactly one owner';
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.table_constraints
   WHERE constraint_schema=DATABASE() AND table_name='conversations'
     AND constraint_name='ck_conversations_single_owner';
  IF object_exists = 0 THEN
    ALTER TABLE conversations ADD CONSTRAINT ck_conversations_single_owner
      CHECK ((user_id IS NOT NULL) + (owner_account_id IS NOT NULL) + (owner_guest_id IS NOT NULL) = 1);
  END IF;

  -- Drop every FK on the retired column, regardless of the name chosen by an older install.
  SELECT COUNT(*) INTO object_exists FROM information_schema.key_column_usage
   WHERE constraint_schema=DATABASE() AND table_name='accounts'
     AND column_name='invited_by_code' AND referenced_table_name IS NOT NULL;
  WHILE object_exists <> 0 DO
    SELECT constraint_name INTO invite_fk_name FROM information_schema.key_column_usage
     WHERE constraint_schema=DATABASE() AND table_name='accounts'
       AND column_name='invited_by_code' AND referenced_table_name IS NOT NULL
     LIMIT 1;
    SET @drop_invite_fk_sql = CONCAT(
      'ALTER TABLE accounts DROP FOREIGN KEY `', REPLACE(invite_fk_name, '`', '``'), '`'
    );
    PREPARE drop_invite_fk FROM @drop_invite_fk_sql;
    EXECUTE drop_invite_fk;
    DEALLOCATE PREPARE drop_invite_fk;
    SELECT COUNT(*) INTO object_exists FROM information_schema.key_column_usage
     WHERE constraint_schema=DATABASE() AND table_name='accounts'
       AND column_name='invited_by_code' AND referenced_table_name IS NOT NULL;
  END WHILE;

  SELECT COUNT(*) INTO object_exists FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='accounts' AND index_name='idx_accounts_invite';
  IF object_exists <> 0 THEN
    DROP INDEX idx_accounts_invite ON accounts;
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='accounts' AND column_name='invited_by_code';
  IF object_exists <> 0 THEN
    ALTER TABLE accounts DROP COLUMN invited_by_code;
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.tables
   WHERE table_schema=DATABASE() AND table_name='invite_codes';
  IF object_exists <> 0 THEN
    DROP TABLE invite_codes;
  END IF;
END$$
DELIMITER ;

CALL migrate_0008();
DROP PROCEDURE migrate_0008;
