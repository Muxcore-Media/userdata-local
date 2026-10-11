package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// matchingBlobKeys uses the authorization boundary's Go whitespace semantics,
// not SQLite TRIM (which does not cover the same Unicode characters). Rows are
// fully consumed and closed before any caller mutates the table.
func matchingBlobKeys(ctx context.Context, tx *sql.Tx, matches func(string) bool) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT user_id FROM user_blobs ORDER BY user_id`)
	if err != nil {
		return nil, fmt.Errorf("list blob keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("read blob key: %w", err)
		}
		if matches(key) {
			keys = append(keys, key)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list blob keys: %w", err)
	}
	return keys, nil
}

// repairAppliedBlobAliases repairs the legacy padded keys that an older
// exact-ID erasure could leave behind even after an OK acknowledgement. Run
// before New returns: an applied+acknowledged tombstone is skipped by the SDK.
// Only padded blob keys are repaired here; applied records and historical
// counts/timestamps, canonical blob keys and parental rows are unchanged.
func (s *Store) repairAppliedBlobAliases(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin erasure blob repair: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	users, err := appliedCanonicalUsers(ctx, tx)
	if err != nil {
		return err
	}
	keys, err := matchingBlobKeys(ctx, tx, func(key string) bool {
		canonical := strings.TrimSpace(key)
		_, applied := users[canonical]
		return key != canonical && applied
	})
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_blobs WHERE user_id = ?`, key); err != nil {
			return fmt.Errorf("repair erased blob alias: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit erasure blob repair: %w", err)
	}
	return nil
}

func appliedCanonicalUsers(ctx context.Context, tx *sql.Tx) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT user_id FROM erasure_applied`)
	if err != nil {
		return nil, fmt.Errorf("list applied users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	users := map[string]struct{}{}
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("read applied user: %w", err)
		}
		if validateBlobUserID(userID) == nil {
			users[userID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list applied users: %w", err)
	}
	return users, nil
}
