-- roster_public_keys records which public_keys rows are owned by the
-- administrator-managed SSH key roster. Rows in this table may be added,
-- reassigned or revoked by the roster reconciliation; public_keys rows absent
-- from this table are considered manual and are never touched by it.
CREATE TABLE IF NOT EXISTS roster_public_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  public_key TEXT NOT NULL UNIQUE,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME NOT NULL,
  UNIQUE (user_id, public_key),
  CONSTRAINT roster_pubkey_user_id_fk
  FOREIGN KEY(user_id) REFERENCES users(id)
  ON DELETE CASCADE
  ON UPDATE CASCADE
);
