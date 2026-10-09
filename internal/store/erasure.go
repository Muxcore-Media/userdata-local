package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ADR-0035 (user erasure ledger) store support: the erasure_applied record,
// the single-transaction EraseUser, the post-condition count and the
// persistent refusal of writes for an erased user id.

// ErrUserErased is returned by every write path for a user id that has an
// erasure_applied record. It is read from the table on every write, so it
// survives restarts and never depends on process memory.
var ErrUserErased = errors.New("user erased")

// AnonymisedActor replaces an erased user id in parental_policies.updated_by.
const AnonymisedActor = "deleted-user"

// Counts keys reported to the identity provider (match [a-z0-9_.-]{1,64}).
const (
	CountUserBlobs                   = "user_blobs"
	CountParentalPolicies            = "parental_policies"
	CountParentalPoliciesAnonymise   = "parental_policies_updated_by"
	CountParentalPoliciesOtherTenant = "parental_policies_other_tenant"
)

func (s *Store) migrateErasure() error {
	// Forward-only and idempotent, like every other migration here.
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS erasure_applied (
		erasure_id  TEXT PRIMARY KEY,
		user_id     TEXT NOT NULL,
		tenant_id   TEXT NOT NULL,
		applied_at  TEXT NOT NULL,
		counts_json TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("migrate erasure_applied: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS erasure_applied_user_id ON erasure_applied (user_id)`); err != nil {
		return fmt.Errorf("migrate erasure_applied index: %w", err)
	}
	return nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func userErasedTx(ctx context.Context, q queryRower, userID string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM erasure_applied WHERE user_id = ? LIMIT 1`, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check erasure record: %w", err)
	}
	return true, nil
}

// UserErased reports whether an erasure_applied record exists for userID.
func (s *Store) UserErased(ctx context.Context, userID string) (bool, error) {
	return userErasedTx(ctx, s.db, userID)
}

// ErasureApplied reports whether erasureID is recorded as applied.
func (s *Store) ErasureApplied(ctx context.Context, erasureID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM erasure_applied WHERE erasure_id = ?`, erasureID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read erasure record: %w", err)
	}
	return true, nil
}

// EraseUser applies one erasure tombstone in ONE transaction and records it
// in erasure_applied; on any error nothing has changed.
//
// Disposition (ADR-0035 §3, userdata-local rows):
//   - the user's user_blobs row is deleted (exact id only; no sibling keys
//     exist at the pins);
//   - the user's parental_policies row is deleted. User ids are globally
//     unique, so deletion is by id: a row for the id under a different tenant
//     than the tombstone's is deleted too and counted separately
//     (CountParentalPoliciesOtherTenant) so the anomaly is visible. The
//     tombstone's tenant is recorded;
//   - every remaining policy whose updated_by is the user is anonymised to
//     AnonymisedActor. Other users' policies are not deleted.
//
// Calling it again for an applied erasure id is a no-op that returns the
// recorded counts.
func (s *Store) EraseUser(ctx context.Context, erasureID, userID, tenantID string) (map[string]int64, error) {
	if erasureID == "" || userID == "" {
		return nil, errors.New("erasure id and user id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin erasure: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prior string
	switch err := tx.QueryRowContext(ctx, `SELECT counts_json FROM erasure_applied WHERE erasure_id = ?`, erasureID).Scan(&prior); {
	case err == nil:
		counts := map[string]int64{}
		if jerr := json.Unmarshal([]byte(prior), &counts); jerr != nil {
			return nil, fmt.Errorf("decode recorded erasure counts: %w", jerr)
		}
		return counts, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("read erasure record: %w", err)
	}

	counts := map[string]int64{}
	exec := func(key, query string, args ...any) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("erase %s: %w", key, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("erase %s: %w", key, err)
		}
		counts[key] += n
		return nil
	}
	if err := exec(CountUserBlobs, `DELETE FROM user_blobs WHERE user_id = ?`, userID); err != nil {
		return nil, err
	}
	if err := exec(CountParentalPolicies, `DELETE FROM parental_policies WHERE user_id = ? AND tenant_id = ?`, userID, tenantID); err != nil {
		return nil, err
	}
	if err := exec(CountParentalPoliciesOtherTenant, `DELETE FROM parental_policies WHERE user_id = ? AND tenant_id <> ?`, userID, tenantID); err != nil {
		return nil, err
	}
	if err := exec(CountParentalPoliciesAnonymise, `UPDATE parental_policies SET updated_by = ? WHERE updated_by = ?`, AnonymisedActor, userID); err != nil {
		return nil, err
	}
	rawCounts, err := json.Marshal(counts)
	if err != nil {
		return nil, fmt.Errorf("encode erasure counts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO erasure_applied (erasure_id, user_id, tenant_id, applied_at, counts_json)
		VALUES (?, ?, ?, ?, ?)`, erasureID, userID, tenantID, time.Now().UTC().Format(time.RFC3339Nano), string(rawCounts)); err != nil {
		return nil, fmt.Errorf("record erasure: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit erasure: %w", err)
	}
	return counts, nil
}

// CountUserRows counts the rows that still carry userID: its user_blobs row,
// its parental_policies rows (any tenant) and policies whose updated_by is the
// user. It is the post-condition of EraseUser and must be 0 afterwards.
func (s *Store) CountUserRows(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM user_blobs WHERE user_id = ?) +
		(SELECT COUNT(*) FROM parental_policies WHERE user_id = ? OR updated_by = ?)`,
		userID, userID, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count user rows: %w", err)
	}
	return n, nil
}
