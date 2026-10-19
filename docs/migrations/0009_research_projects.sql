-- ============================================================================
-- 研究项目成为 Web 会话边界 (MySQL 8.0+)
-- ADR 0017 / Spec 0004
--
-- !!! 显式手工迁移 / 破坏性警告 !!!
-- 执行前必须停止旧 Web 写入，创建并验证可恢复的数据库备份。
-- 本迁移会永久删除 owner_account_id 或 owner_guest_id 归属的旧 Web
-- conversations 及其 messages，并清空旧 Web 当前会话/导入结果指针。
-- 它绝不删除 user_id 非空的 QQ conversations/messages，不删除 papers，且绝不
-- 修改 guest_sessions.deep_read_state、reservation_id 或 reserved_at。
--
-- MySQL DDL 会隐式提交；整份文件不是可整体回滚的事务。失败时应保持停写，检查
-- 已完成步骤后重跑本文件或从备份恢复，不能继续运行旧应用。ALTER/DROP 均通过
-- information_schema 检查，避免依赖 IF EXISTS 的版本差异。
-- ============================================================================

SET NAMES utf8mb4;

-- 项目先引用已存在的 account/guest；active_conversation_id 是循环且 owner-scoped
-- 的应用指针，故有意不建 FK，由项目/会话事务锁定并校验。
CREATE TABLE IF NOT EXISTS projects (
  id                     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  owner_account_id       BIGINT UNSIGNED     NULL,
  owner_guest_id         BIGINT UNSIGNED     NULL,
  project_no             INT UNSIGNED    NOT NULL COMMENT 'owner 范围内稳定展示编号',
  title                  VARCHAR(128)    NOT NULL,
  active_conversation_id BIGINT UNSIGNED     NULL COMMENT '应用事务校验必须属于本项目且 active',
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

DROP PROCEDURE IF EXISTS migrate_0009_core;
DELIMITER $$
CREATE PROCEDURE migrate_0009_core()
BEGIN
  DECLARE object_exists INT DEFAULT 0;
  DECLARE old_owner_columns INT DEFAULT 0;
  DECLARE invalid_rows BIGINT DEFAULT 0;
  DECLARE orphan_rows BIGINT DEFAULT 0;
  DECLARE dependency_name VARCHAR(64);

  SELECT COUNT(*) INTO object_exists
    FROM information_schema.tables
   WHERE table_schema=DATABASE()
     AND table_name IN ('accounts','guest_sessions','users','conversations','papers','messages');
  IF object_exists <> 6 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='0009 refused: expected the complete 0008 base schema';
  END IF;

  SELECT COUNT(*) INTO old_owner_columns
    FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='conversations'
     AND column_name IN ('owner_account_id','owner_guest_id');
  IF old_owner_columns = 1 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='0009 refused: conversations has only one legacy Web owner column';
  END IF;

  -- First-run destructive cleanup. The user_id IS NULL guard makes the QQ exclusion
  -- explicit even if an old installation somehow violated its three-owner CHECK.
  IF old_owner_columns = 2 THEN
    SELECT COUNT(*) INTO object_exists
      FROM information_schema.columns
     WHERE table_schema=DATABASE() AND table_name='accounts'
       AND column_name='active_conversation_id';
    IF object_exists <> 0 THEN
      UPDATE accounts a
      JOIN conversations c ON c.id=a.active_conversation_id
         SET a.active_conversation_id=NULL
       WHERE c.user_id IS NULL
         AND (c.owner_account_id IS NOT NULL OR c.owner_guest_id IS NOT NULL);
    END IF;

    SELECT COUNT(*) INTO object_exists
      FROM information_schema.columns
     WHERE table_schema=DATABASE() AND table_name='guest_sessions'
       AND column_name='active_conversation_id';
    IF object_exists <> 0 THEN
      UPDATE guest_sessions g
      JOIN conversations c ON c.id=g.active_conversation_id
         SET g.active_conversation_id=NULL
       WHERE c.user_id IS NULL
         AND (c.owner_account_id IS NOT NULL OR c.owner_guest_id IS NOT NULL);
    END IF;

    -- Old import result IDs point at conversations that are about to be deleted; they
    -- must not be relabelled as project IDs. The idempotency key itself is retained.
    SELECT COUNT(*) INTO object_exists
      FROM information_schema.columns
     WHERE table_schema=DATABASE() AND table_name='guest_sessions'
       AND column_name='imported_conversation_ids';
    IF object_exists <> 0 THEN
      UPDATE guest_sessions SET imported_conversation_ids=NULL;
    END IF;

    SELECT COUNT(*) INTO object_exists
      FROM information_schema.columns
     WHERE table_schema=DATABASE() AND table_name='guest_sessions'
       AND column_name='imported_current_conversation_id';
    IF object_exists <> 0 THEN
      UPDATE guest_sessions SET imported_current_conversation_id=NULL;
    END IF;

    DELETE m
      FROM messages m
      JOIN conversations c ON c.id=m.conversation_id
     WHERE c.user_id IS NULL
       AND (c.owner_account_id IS NOT NULL OR c.owner_guest_id IS NOT NULL);

    DELETE FROM conversations
     WHERE user_id IS NULL
       AND (owner_account_id IS NOT NULL OR owner_guest_id IS NOT NULL);
  END IF;

  -- New account/guest project pointers. They intentionally remain nullable and have no
  -- FK because a simple FK cannot prove same-owner membership.
  SELECT COUNT(*) INTO object_exists
    FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='accounts' AND column_name='active_project_id';
  IF object_exists = 0 THEN
    ALTER TABLE accounts
      ADD COLUMN active_project_id BIGINT UNSIGNED NULL AFTER qq_openid;
  END IF;

  SELECT COUNT(*) INTO object_exists
    FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='guest_sessions' AND column_name='active_project_id';
  IF object_exists = 0 THEN
    ALTER TABLE guest_sessions
      ADD COLUMN active_project_id BIGINT UNSIGNED NULL AFTER upgraded_to_account_id;
  END IF;

  SELECT COUNT(*) INTO object_exists
    FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='guest_sessions' AND column_name='imported_project_ids';
  IF object_exists = 0 THEN
    ALTER TABLE guest_sessions
      ADD COLUMN imported_project_ids JSON NULL AFTER import_idempotency_key;
  END IF;

  SELECT COUNT(*) INTO object_exists
    FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='guest_sessions' AND column_name='imported_current_project_id';
  IF object_exists = 0 THEN
    ALTER TABLE guest_sessions
      ADD COLUMN imported_current_project_id BIGINT UNSIGNED NULL AFTER imported_project_ids;
  END IF;

  -- Drop every FK using each retired active/import column, regardless of custom name.
  SELECT COUNT(*) INTO object_exists
    FROM information_schema.key_column_usage
   WHERE constraint_schema=DATABASE() AND table_name='accounts'
     AND column_name='active_conversation_id' AND referenced_table_name IS NOT NULL;
  WHILE object_exists <> 0 DO
    SELECT constraint_name INTO dependency_name
      FROM information_schema.key_column_usage
     WHERE constraint_schema=DATABASE() AND table_name='accounts'
       AND column_name='active_conversation_id' AND referenced_table_name IS NOT NULL
     LIMIT 1;
    SET @sql_0009=CONCAT('ALTER TABLE accounts DROP FOREIGN KEY `',
                         REPLACE(dependency_name,'`','``'),'`');
    PREPARE stmt_0009 FROM @sql_0009; EXECUTE stmt_0009; DEALLOCATE PREPARE stmt_0009;
    SELECT COUNT(*) INTO object_exists
      FROM information_schema.key_column_usage
     WHERE constraint_schema=DATABASE() AND table_name='accounts'
       AND column_name='active_conversation_id' AND referenced_table_name IS NOT NULL;
  END WHILE;

  SELECT COUNT(*) INTO object_exists
    FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='accounts' AND column_name='active_conversation_id';
  IF object_exists <> 0 THEN
    ALTER TABLE accounts DROP COLUMN active_conversation_id;
  END IF;

  SELECT COUNT(*) INTO object_exists
    FROM information_schema.key_column_usage
   WHERE constraint_schema=DATABASE() AND table_name='guest_sessions'
     AND column_name IN ('active_conversation_id','imported_current_conversation_id')
     AND referenced_table_name IS NOT NULL;
  WHILE object_exists <> 0 DO
    SELECT constraint_name INTO dependency_name
      FROM information_schema.key_column_usage
     WHERE constraint_schema=DATABASE() AND table_name='guest_sessions'
       AND column_name IN ('active_conversation_id','imported_current_conversation_id')
       AND referenced_table_name IS NOT NULL
     LIMIT 1;
    SET @sql_0009=CONCAT('ALTER TABLE guest_sessions DROP FOREIGN KEY `',
                         REPLACE(dependency_name,'`','``'),'`');
    PREPARE stmt_0009 FROM @sql_0009; EXECUTE stmt_0009; DEALLOCATE PREPARE stmt_0009;
    SELECT COUNT(*) INTO object_exists
      FROM information_schema.key_column_usage
     WHERE constraint_schema=DATABASE() AND table_name='guest_sessions'
       AND column_name IN ('active_conversation_id','imported_current_conversation_id')
       AND referenced_table_name IS NOT NULL;
  END WHILE;

  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='guest_sessions' AND column_name='active_conversation_id';
  IF object_exists <> 0 THEN
    ALTER TABLE guest_sessions DROP COLUMN active_conversation_id;
  END IF;
  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='guest_sessions' AND column_name='imported_conversation_ids';
  IF object_exists <> 0 THEN
    ALTER TABLE guest_sessions DROP COLUMN imported_conversation_ids;
  END IF;
  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='guest_sessions' AND column_name='imported_current_conversation_id';
  IF object_exists <> 0 THEN
    ALTER TABLE guest_sessions DROP COLUMN imported_current_conversation_id;
  END IF;

  -- Add project_id before removing legacy Web ownership dependencies.
  SELECT COUNT(*) INTO object_exists
    FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='conversations' AND column_name='project_id';
  IF object_exists = 0 THEN
    ALTER TABLE conversations ADD COLUMN project_id BIGINT UNSIGNED NULL AFTER user_id;
  END IF;

  -- All FKs on retired owner columns must be removed before their backing indexes/columns.
  SELECT COUNT(*) INTO object_exists
    FROM information_schema.key_column_usage
   WHERE constraint_schema=DATABASE() AND table_name='conversations'
     AND column_name IN ('owner_account_id','owner_guest_id')
     AND referenced_table_name IS NOT NULL;
  WHILE object_exists <> 0 DO
    SELECT constraint_name INTO dependency_name
      FROM information_schema.key_column_usage
     WHERE constraint_schema=DATABASE() AND table_name='conversations'
       AND column_name IN ('owner_account_id','owner_guest_id')
       AND referenced_table_name IS NOT NULL
     LIMIT 1;
    SET @sql_0009=CONCAT('ALTER TABLE conversations DROP FOREIGN KEY `',
                         REPLACE(dependency_name,'`','``'),'`');
    PREPARE stmt_0009 FROM @sql_0009; EXECUTE stmt_0009; DEALLOCATE PREPARE stmt_0009;
    SELECT COUNT(*) INTO object_exists
      FROM information_schema.key_column_usage
     WHERE constraint_schema=DATABASE() AND table_name='conversations'
       AND column_name IN ('owner_account_id','owner_guest_id')
       AND referenced_table_name IS NOT NULL;
  END WHILE;

  -- Drop every CHECK that mentions a retired owner column, including custom names.
  SELECT COUNT(*) INTO object_exists
    FROM information_schema.table_constraints tc
    JOIN information_schema.check_constraints cc
      ON cc.constraint_schema=tc.constraint_schema AND cc.constraint_name=tc.constraint_name
   WHERE tc.constraint_schema=DATABASE() AND tc.table_name='conversations'
     AND tc.constraint_type='CHECK'
     AND (LOWER(cc.check_clause) LIKE '%owner_account_id%'
       OR LOWER(cc.check_clause) LIKE '%owner_guest_id%');
  WHILE object_exists <> 0 DO
    SELECT tc.constraint_name INTO dependency_name
      FROM information_schema.table_constraints tc
      JOIN information_schema.check_constraints cc
        ON cc.constraint_schema=tc.constraint_schema AND cc.constraint_name=tc.constraint_name
     WHERE tc.constraint_schema=DATABASE() AND tc.table_name='conversations'
       AND tc.constraint_type='CHECK'
       AND (LOWER(cc.check_clause) LIKE '%owner_account_id%'
         OR LOWER(cc.check_clause) LIKE '%owner_guest_id%')
     LIMIT 1;
    SET @sql_0009=CONCAT('ALTER TABLE conversations DROP CHECK `',
                         REPLACE(dependency_name,'`','``'),'`');
    PREPARE stmt_0009 FROM @sql_0009; EXECUTE stmt_0009; DEALLOCATE PREPARE stmt_0009;
    SELECT COUNT(*) INTO object_exists
      FROM information_schema.table_constraints tc
      JOIN information_schema.check_constraints cc
        ON cc.constraint_schema=tc.constraint_schema AND cc.constraint_name=tc.constraint_name
     WHERE tc.constraint_schema=DATABASE() AND tc.table_name='conversations'
       AND tc.constraint_type='CHECK'
       AND (LOWER(cc.check_clause) LIKE '%owner_account_id%'
         OR LOWER(cc.check_clause) LIKE '%owner_guest_id%');
  END WHILE;

  -- Drop all non-primary indexes containing a retired owner column. This covers the
  -- canonical 0008 names and any index name chosen by an older/manual installation.
  SELECT COUNT(DISTINCT index_name) INTO object_exists
    FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='conversations'
     AND column_name IN ('owner_account_id','owner_guest_id') AND index_name <> 'PRIMARY';
  WHILE object_exists <> 0 DO
    SELECT index_name INTO dependency_name
      FROM information_schema.statistics
     WHERE table_schema=DATABASE() AND table_name='conversations'
       AND column_name IN ('owner_account_id','owner_guest_id') AND index_name <> 'PRIMARY'
     LIMIT 1;
    SET @sql_0009=CONCAT('ALTER TABLE conversations DROP INDEX `',
                         REPLACE(dependency_name,'`','``'),'`');
    PREPARE stmt_0009 FROM @sql_0009; EXECUTE stmt_0009; DEALLOCATE PREPARE stmt_0009;
    SELECT COUNT(DISTINCT index_name) INTO object_exists
      FROM information_schema.statistics
     WHERE table_schema=DATABASE() AND table_name='conversations'
       AND column_name IN ('owner_account_id','owner_guest_id') AND index_name <> 'PRIMARY';
  END WHILE;

  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='conversations' AND column_name='owner_account_id';
  IF object_exists <> 0 THEN
    ALTER TABLE conversations DROP COLUMN owner_account_id;
  END IF;
  SELECT COUNT(*) INTO object_exists FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='conversations' AND column_name='owner_guest_id';
  IF object_exists <> 0 THEN
    ALTER TABLE conversations DROP COLUMN owner_guest_id;
  END IF;

  SELECT COUNT(*) INTO invalid_rows
    FROM conversations
   WHERE (user_id IS NOT NULL) + (project_id IS NOT NULL) <> 1;
  IF invalid_rows <> 0 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='0009 refused: conversations violates user_id XOR project_id';
  END IF;

  SELECT COUNT(*) INTO object_exists
    FROM information_schema.table_constraints
   WHERE constraint_schema=DATABASE() AND table_name='conversations'
     AND constraint_name='ck_conversations_single_owner';
  IF object_exists = 0 THEN
    ALTER TABLE conversations ADD CONSTRAINT ck_conversations_single_owner
      CHECK ((user_id IS NOT NULL) + (project_id IS NOT NULL) = 1);
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='conversations'
     AND index_name='uk_conversations_project_no';
  IF object_exists = 0 THEN
    CREATE UNIQUE INDEX uk_conversations_project_no
      ON conversations (project_id, conversation_no);
  END IF;

  SELECT COUNT(*) INTO object_exists FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='conversations'
     AND index_name='idx_conversations_project';
  IF object_exists = 0 THEN
    CREATE INDEX idx_conversations_project
      ON conversations (project_id, status, updated_at, id);
  END IF;

  -- Preflight existing rows before adding mandatory ownership FKs.
  SELECT COUNT(*) INTO orphan_rows
    FROM conversations c LEFT JOIN users u ON u.id=c.user_id
   WHERE c.user_id IS NOT NULL AND u.id IS NULL;
  IF orphan_rows <> 0 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='0009 refused: orphan QQ conversation user_id';
  END IF;

  SELECT COUNT(*) INTO orphan_rows
    FROM conversations c LEFT JOIN projects p ON p.id=c.project_id
   WHERE c.project_id IS NOT NULL AND p.id IS NULL;
  IF orphan_rows <> 0 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='0009 refused: orphan conversation project_id';
  END IF;

  SELECT COUNT(*) INTO orphan_rows
    FROM messages m LEFT JOIN conversations c ON c.id=m.conversation_id
   WHERE c.id IS NULL;
  IF orphan_rows <> 0 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='0009 refused: orphan message conversation_id';
  END IF;

  SELECT COUNT(*) INTO object_exists
    FROM information_schema.key_column_usage
   WHERE constraint_schema=DATABASE() AND table_name='conversations'
     AND column_name='user_id' AND referenced_table_name='users' AND referenced_column_name='id';
  IF object_exists = 0 THEN
    ALTER TABLE conversations ADD CONSTRAINT fk_conversations_user
      FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
  END IF;

  SELECT COUNT(*) INTO object_exists
    FROM information_schema.key_column_usage
   WHERE constraint_schema=DATABASE() AND table_name='conversations'
     AND column_name='project_id' AND referenced_table_name='projects' AND referenced_column_name='id';
  IF object_exists = 0 THEN
    ALTER TABLE conversations ADD CONSTRAINT fk_conversations_project
      FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE;
  END IF;

  SELECT COUNT(*) INTO object_exists
    FROM information_schema.key_column_usage
   WHERE constraint_schema=DATABASE() AND table_name='messages'
     AND column_name='conversation_id' AND referenced_table_name='conversations'
     AND referenced_column_name='id';
  IF object_exists = 0 THEN
    ALTER TABLE messages ADD CONSTRAINT fk_messages_conversation
      FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE;
  END IF;
END$$
DELIMITER ;

CALL migrate_0009_core();
DROP PROCEDURE migrate_0009_core;

-- 项目论文是可软移出的稳定关系；删除 owner/project 时级联清理，永不反向删除
-- 全局 papers。
CREATE TABLE IF NOT EXISTS project_papers (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  project_id  BIGINT UNSIGNED NOT NULL,
  paper_id    BIGINT UNSIGNED NOT NULL,
  status      ENUM('active','removed') NOT NULL DEFAULT 'active',
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_project_papers_identity (project_id, paper_id),
  KEY idx_project_papers_list (project_id, status, updated_at, id),
  KEY idx_project_papers_paper (paper_id),
  CONSTRAINT fk_project_papers_project FOREIGN KEY (project_id)
    REFERENCES projects(id) ON DELETE CASCADE,
  CONSTRAINT fk_project_papers_paper FOREIGN KEY (paper_id)
    REFERENCES papers(id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='项目对全局论文的可恢复引用';

-- 结果只引用成功 bot 消息，不复制正文。消息 role、消息/来源会话一致性、来源会话
-- 与项目论文同项目由精读短事务锁定并校验。
CREATE TABLE IF NOT EXISTS project_read_results (
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

-- Post-checks are read-only. Operators must compare the preserved QQ/deep-read snapshots
-- taken before migration; these counts provide immediate evidence that no legacy Web rows remain.
SELECT COUNT(*) AS legacy_web_conversations_remaining
  FROM conversations
 WHERE project_id IS NULL AND user_id IS NULL;

SELECT COUNT(*) AS qq_conversations_preserved
  FROM conversations
 WHERE user_id IS NOT NULL AND project_id IS NULL;

SELECT deep_read_state, COUNT(*) AS guest_count
  FROM guest_sessions
 GROUP BY deep_read_state
 ORDER BY deep_read_state;
