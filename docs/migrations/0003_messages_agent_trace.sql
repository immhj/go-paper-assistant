-- Run before deploying the dual-Agent application against an existing database.
-- Existing rows remain NULL and old binaries remain compatible because all SQL uses explicit columns.
ALTER TABLE messages
  ADD COLUMN agent_trace JSON NULL COMMENT 'Versioned Router/Executor execution summary; no secrets, prompts, or hidden reasoning',
  ALGORITHM=INSTANT;
