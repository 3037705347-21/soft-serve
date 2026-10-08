CREATE TABLE IF NOT EXISTS webhook_pending_deliveries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  webhook_id INTEGER NOT NULL,
  event INTEGER NOT NULL,
  event_key TEXT NOT NULL,
  request_body TEXT NOT NULL,
  status INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_retry_at INTEGER NOT NULL,
  claimed_at INTEGER,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT webhook_pending_webhook_id_fk
  FOREIGN KEY(webhook_id) REFERENCES webhooks(id)
  ON DELETE CASCADE
  ON UPDATE CASCADE
);

-- Only one outstanding (pending or in-flight) record per webhook/event.
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_pending_deliveries_unique
  ON webhook_pending_deliveries (webhook_id, event_key)
  WHERE status IN (0, 1);

CREATE INDEX IF NOT EXISTS idx_webhook_pending_deliveries_due
  ON webhook_pending_deliveries (status, next_retry_at);
