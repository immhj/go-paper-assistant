-- Adds observable delivery state for user messages and reserved bot replies.
ALTER TABLE messages
  ADD COLUMN delivery_status ENUM('received','reserved','sent','failed') NOT NULL DEFAULT 'received'
  COMMENT 'User=received; bot reply reservation, delivered, or failed',
  ALGORITHM=INSTANT;

-- Existing bot rows predate delivery tracking and represent persisted historical replies.
UPDATE messages SET delivery_status='sent' WHERE role='bot';
