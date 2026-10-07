package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/Muxcore-Media/userdata-local/parental"
)

var ErrPolicyConflict = errors.New("parental policy revision conflict")

func (s *Store) migrateParentalPolicies() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS parental_policies (
		tenant_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		policy_json BLOB NOT NULL,
		revision INTEGER NOT NULL CHECK (revision > 0),
		updated_by TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (tenant_id, user_id)
	)`)
	if err != nil {
		return fmt.Errorf("migrate parental policies: %w", err)
	}
	return nil
}

// GetParentalPolicy distinguishes absent rows from corrupt/unavailable storage.
// Legacy user_blobs (including prefs.parental) are never read here.
func (s *Store) GetParentalPolicy(ctx context.Context, scope parental.Scope) (parental.Document, error) {
	if err := parental.ValidateScope(scope); err != nil {
		return parental.Document{}, err
	}
	row := s.db.QueryRowContext(ctx, `SELECT policy_json, revision, updated_at
		FROM parental_policies WHERE tenant_id = ? AND user_id = ?`, scope.TenantID, scope.UserID)
	var raw []byte
	var revision int64
	var updatedAt string
	if err := row.Scan(&raw, &revision, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return parental.Unconfigured(scope), nil
		}
		return parental.Document{}, err
	}
	policy, err := parental.DecodePolicy(raw)
	if err != nil {
		return parental.Document{}, fmt.Errorf("invalid stored parental policy: %w", err)
	}
	if revision <= 0 {
		return parental.Document{}, errors.New("invalid stored parental revision")
	}
	if _, err := time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return parental.Document{}, errors.New("invalid stored parental timestamp")
	}
	return parental.Document{Scope: scope, State: "configured", Revision: revision, Policy: &policy, UpdatedAt: updatedAt}, nil
}

// PutParentalPolicy uses database compare-and-swap, including competing Store
// instances/processes. It cannot promote or change ordinary userdata blobs.
func (s *Store) PutParentalPolicy(ctx context.Context, scope parental.Scope, actor string, update parental.Update) (parental.Document, error) {
	if err := parental.ValidateScope(scope); err != nil {
		return parental.Document{}, err
	}
	if err := parental.ValidateScope(parental.Scope{UserID: actor}); err != nil {
		return parental.Document{}, err
	}
	if update.ExpectedRevision < 0 || update.ExpectedRevision == math.MaxInt64 {
		return parental.Document{}, errors.New("invalid expected parental revision")
	}
	policy, err := parental.Normalize(update.Policy)
	if err != nil {
		return parental.Document{}, err
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return parental.Document{}, err
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return parental.Document{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var result sql.Result
	if update.ExpectedRevision == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO parental_policies
			(tenant_id, user_id, policy_json, revision, updated_by, updated_at)
			VALUES (?, ?, ?, 1, ?, ?) ON CONFLICT(tenant_id, user_id) DO NOTHING`,
			scope.TenantID, scope.UserID, raw, actor, updatedAt)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE parental_policies SET
			policy_json = ?, revision = revision + 1, updated_by = ?, updated_at = ?
			WHERE tenant_id = ? AND user_id = ? AND revision = ?`,
			raw, actor, updatedAt, scope.TenantID, scope.UserID, update.ExpectedRevision)
	}
	if err != nil {
		return parental.Document{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return parental.Document{}, err
	}
	if changed != 1 {
		return parental.Document{}, ErrPolicyConflict
	}
	if err := tx.Commit(); err != nil {
		return parental.Document{}, err
	}
	return parental.Document{Scope: scope, State: "configured", Revision: update.ExpectedRevision + 1, Policy: &policy, UpdatedAt: updatedAt}, nil
}
