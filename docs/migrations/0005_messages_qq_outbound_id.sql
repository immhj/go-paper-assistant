-- Stores the QQ bot message/stream ID returned by the platform for quote resolution.
ALTER TABLE messages
  ADD COLUMN qq_outbound_id VARCHAR(128) NULL
  COMMENT 'QQ bot outbound message or stream ID used by quoted replies',
  ALGORITHM=INSTANT;

CREATE INDEX idx_messages_outbound ON messages (qq_outbound_id);
