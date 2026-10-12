package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/Muxcore-Media/userdata-local/conditional"
)

// GetConditional returns a raw, scoped read without creating a row or running
// legacy defaults/merge. An absent row is not proof of never-written state.
func (s *Store) GetConditional(ctx context.Context, userID string) (conditional.Document, error) {
	if !conditional.ValidID(userID) {
		return conditional.Document{}, conditional.InvalidPrecondition
	}
	raw, revision, err := readConditional(ctx, s.db, userID)
	if err != nil {
		return conditional.Document{}, err
	}
	document := s.conditionalDocument(userID, raw, revision)
	if _, err := conditional.EncodeDocument(document); err != nil {
		return conditional.Document{}, conditional.StorageUnavailable
	}
	return document, nil
}

func readConditional(ctx context.Context, q queryRower, userID string) (json.RawMessage, int64, error) {
	var raw []byte
	var revision int64
	err := q.QueryRowContext(ctx, "SELECT json_blob, revision FROM user_blobs WHERE user_id = ?", userID).Scan(&raw, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return conditional.InitialBlob(), 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if revision <= 0 || conditional.ValidateBlob(raw) != nil {
		return nil, 0, conditional.StorageUnavailable
	}
	return raw, revision, nil
}

func (s *Store) conditionalDocument(userID string, raw json.RawMessage, revision int64) conditional.Document {
	return conditional.Document{
		Contract: conditional.Capability, Authority: s.authority, UserID: userID, TenantID: "",
		Revision: strconv.FormatInt(revision, 10), Exists: revision > 0,
		Operations: conditional.Operations(), LegacyWrites: true, Blob: raw,
	}
}

// PutConditional serializes the complete read/check/apply/write transaction in
// SQLite, including across connections. It has no receipt/rebase/retry behavior.
// Legacy writers remain unrestricted and can later replace unknown values.
func (s *Store) PutConditional(ctx context.Context, userID string, update conditional.Update) (conditional.Document, error) {
	if err := conditional.ValidateUpdate(update); err != nil {
		return conditional.Document{}, err
	}
	if !conditional.ValidID(userID) || update.Expected.UserID != userID || update.Expected.TenantID != "" {
		return conditional.Document{}, conditional.InvalidPrecondition
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return conditional.Document{}, err
	}
	defer func() { _ = conn.Close() }()
	// Set this on the reserved connection: the legacy DSN's parameter is not
	// relied upon to configure modernc's SQLite busy handler.
	busyMS := int64(5000)
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return conditional.Document{}, context.DeadlineExceeded
		}
		if milliseconds := (remaining + time.Millisecond - 1) / time.Millisecond; int64(milliseconds) < busyMS {
			busyMS = int64(milliseconds)
		}
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busyMS)); err != nil {
		return conditional.Document{}, err
	}
	committed := false
	// Use a fresh, bounded context: a canceled request must not leave an open
	// transaction in the pool. Discard a connection whose rollback is uncertain.
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return conditional.Document{}, err
	}
	raw, revision, err := readConditional(ctx, conn, userID)
	if err != nil {
		return conditional.Document{}, err
	}
	erased, err := s.writeUserErasedLocked(ctx, conn, userID)
	if err != nil {
		return conditional.Document{}, err
	}
	if erased {
		return conditional.Document{}, ErrUserErased
	}
	if update.Expected.Authority != s.authority {
		return conditional.Document{}, conditional.AuthorityConflict
	}
	expected, _ := conditional.ParseRevision(update.Expected.Revision)
	if expected != revision {
		return conditional.Document{}, conditional.RevisionConflict
	}
	if revision == math.MaxInt64 {
		return conditional.Document{}, conditional.RevisionExhausted
	}
	merged, err := conditional.Apply(raw, update.Mutation)
	if err != nil {
		return conditional.Document{}, err
	}
	document := s.conditionalDocument(userID, merged, revision+1)
	document.Outcome, document.RequestID, document.PreviousRevision = "applied", update.RequestID, update.Expected.Revision
	if _, err := conditional.EncodeDocument(document); err != nil {
		return conditional.Document{}, err
	}
	var result sql.Result
	if revision == 0 {
		result, err = conn.ExecContext(ctx, `
			INSERT INTO user_blobs (user_id, json_blob, revision, updated_at)
			VALUES (?, ?, 1, datetime('now')) ON CONFLICT(user_id) DO NOTHING`, userID, []byte(merged))
	} else {
		result, err = conn.ExecContext(ctx, `
			UPDATE user_blobs SET json_blob = ?, revision = revision + 1, updated_at = datetime('now')
			WHERE user_id = ? AND revision = ?`, []byte(merged), userID, revision)
	}
	if err != nil {
		return conditional.Document{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return conditional.Document{}, err
	}
	if changed != 1 {
		return conditional.Document{}, conditional.RevisionConflict
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return conditional.Document{}, err
	}
	committed = true
	return document, nil
}
