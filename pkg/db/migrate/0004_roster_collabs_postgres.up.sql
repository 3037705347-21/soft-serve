-- roster_collabs records which collabs rows are owned by the
-- administrator-managed repository access matrix. Rows in this table may be
-- granted, updated or revoked by the matrix reconciliation; collabs rows
-- absent from this table are considered manual and are never touched by it.
CREATE TABLE IF NOT EXISTS roster_collabs (
  id SERIAL PRIMARY KEY,
  repo_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  access_level INTEGER NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP NOT NULL,
  UNIQUE (repo_id, user_id),
  CONSTRAINT roster_collab_repo_id_fk
  FOREIGN KEY(repo_id) REFERENCES repos(id)
  ON DELETE CASCADE
  ON UPDATE CASCADE,
  CONSTRAINT roster_collab_user_id_fk
  FOREIGN KEY(user_id) REFERENCES users(id)
  ON DELETE CASCADE
  ON UPDATE CASCADE
);
