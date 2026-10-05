CREATE TABLE user_blobs (
			user_id    TEXT PRIMARY KEY,
			json_blob  BLOB NOT NULL,
			revision   INTEGER NOT NULL DEFAULT 1,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
