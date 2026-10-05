# Upgrade snapshots

Fixtures for `../../upgrade_test.go` (ADR-0015, NFR-DATA-002, FR-INS-005).

- `v0.1.0.db` / `v0.1.0.schema.sql`: produced by the v0.1.0 tag (the only tag of this module).
- Produced with a `git worktree` of `v0.1.0`, adding `seed_upgrade_test.go.txt` as
  `internal/store/seed_upgrade_test.go` (build tag `upgradeseed`), then:
  `UPGRADE_SEED_DB=/tmp/seed.db GOWORK=off go test -tags upgradeseed -run TestUpgradeSeed ./internal/store/`
  followed by `sqlite3 /tmp/seed.db VACUUM`.
- Seed: table `user_blobs` with two rows. `alice@example.com` (revision 2, theme `light`,
  subtitle language `fra`, parental prefs including a PIN hash) and `bob` (revision 1,
  default prefs). Each has two progress entries, one favorite, one playlist, one queue item.
