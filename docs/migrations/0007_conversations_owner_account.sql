-- ============================================================================
-- Web 聊天通道 · 会话归属扩展   (MySQL 8.0+)
--
-- 论文助手原本的会话只按 QQ 用户归属（conversations.user_id -> users.id）。
-- Web 聊天通道（ADR 0013、Spec 0002）让会话可以改按平台账号归属。为此给
-- conversations 增加一个可空的 owner_account_id：
--
--   QQ 通道产生的会话：user_id 非空、owner_account_id 为空
--   Web 通道产生的会话：owner_account_id 非空、user_id 为空
--
-- 「恰好一个非空」这条不变量由应用层保证（单实例、Go 侧统一写入，ADR 0006）。
-- 也可用 CHECK 约束在数据库层强制，见文件末尾（默认注释，避免影响已有行）。
-- ============================================================================

ALTER TABLE conversations
  ADD COLUMN owner_account_id BIGINT UNSIGNED NULL
  COMMENT 'Web 通道会话归属的平台账号；与 user_id 二选一，恰好一个非空'
  AFTER user_id,
  ALGORITHM=INSTANT;

-- 已存在的会话都是 QQ 通道产生的，user_id 已经非空，owner_account_id 保持 NULL 即正确。

CREATE INDEX idx_conversations_account ON conversations (owner_account_id, status, updated_at);

-- 现有的 uk_conversations_no (user_id, conversation_no) 只覆盖 QQ 会话（owner_account_id 为空）。
-- 账号会话的每账号稳定编号唯一性由应用层保证；需要数据库级唯一时可另建
-- 唯一索引 (owner_account_id, conversation_no)。MySQL 唯一索引允许多个 NULL，两者互不冲突。

-- ============================================================================
-- 可选：数据库层强制「恰好一个归属非空」。默认注释，取消注释前需确认所有历史行满足。
-- ============================================================================
-- ALTER TABLE conversations
--   ADD CONSTRAINT ck_conversations_single_owner
--   CHECK ((user_id IS NOT NULL) <> (owner_account_id IS NOT NULL));
-- ALTER TABLE conversations ADD CONSTRAINT fk_conversations_account
--   FOREIGN KEY (owner_account_id) REFERENCES accounts(id) ON DELETE CASCADE;
