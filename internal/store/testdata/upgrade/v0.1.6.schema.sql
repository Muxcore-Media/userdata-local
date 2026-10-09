CREATE TABLE user_blobs (
			user_id    TEXT PRIMARY KEY,
			json_blob  BLOB NOT NULL,
			revision   INTEGER NOT NULL DEFAULT 1,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
CREATE TABLE parental_policies (
		tenant_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		policy_json BLOB NOT NULL,
		revision INTEGER NOT NULL CHECK (revision > 0),
		updated_by TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (tenant_id, user_id)
	);
