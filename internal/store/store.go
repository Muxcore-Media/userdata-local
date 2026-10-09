package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/Muxcore-Media/userdata-local/internal/models"
)

// ErrBlobTooLarge is returned by Put when the merged blob would exceed
// models.MaxBlobBytes. The stored blob and revision are left unchanged.
var ErrBlobTooLarge = errors.New("userdata blob too large")

// Store persists per-user userdata blobs in SQLite.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// New opens or creates the SQLite database.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS user_blobs (
			user_id    TEXT PRIMARY KEY,
			json_blob  BLOB NOT NULL,
			revision   INTEGER NOT NULL DEFAULT 1,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := s.migrateParentalPolicies(); err != nil {
		return err
	}
	return s.migrateErasure()
}

// persistBlobLocked upserts the blob in one transaction that first refuses a
// user with an applied erasure (ADR-0035), so a late write cannot resurrect
// erased data. The caller holds s.mu.
func (s *Store) persistBlobLocked(ctx context.Context, userID string, raw []byte, revision int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("persist blob: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	erased, err := userErasedTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if erased {
		return ErrUserErased
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_blobs (user_id, json_blob, revision, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT(user_id) DO UPDATE SET
			json_blob = excluded.json_blob,
			revision = excluded.revision,
			updated_at = datetime('now')`,
		userID, raw, revision,
	); err != nil {
		return fmt.Errorf("persist blob: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("persist blob: %w", err)
	}
	return nil
}

// Close closes the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// Get returns the stored blob for userID.
func (s *Store) Get(userID string) (models.Blob, int64, error) {
	if userID == "" {
		return models.Blob{}, 0, fmt.Errorf("user_id is required")
	}
	row := s.db.QueryRow(`SELECT json_blob, revision FROM user_blobs WHERE user_id = ?`, userID)
	var raw []byte
	var revision int64
	if err := row.Scan(&raw, &revision); err != nil {
		if err == sql.ErrNoRows {
			return models.EmptyBlob(), 0, nil
		}
		return models.Blob{}, 0, err
	}
	blob, err := ParseBlob(raw)
	if err != nil {
		return models.Blob{}, 0, fmt.Errorf("decode blob: %w", err)
	}
	return blob, revision, nil
}

// Put merges incoming into stored state and persists the result.
func (s *Store) Put(userID string, incoming models.Blob) (models.Blob, int64, error) {
	if userID == "" {
		return models.Blob{}, 0, fmt.Errorf("user_id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stored, revision, err := s.getLocked(userID)
	if err != nil {
		return models.Blob{}, 0, err
	}
	merged := MergeBlob(stored, incoming)
	for id, entry := range incoming.Progress {
		merged.Progress[id] = NormalizeProgress(entry)
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return models.Blob{}, 0, err
	}
	if len(raw) > models.MaxBlobBytes {
		return models.Blob{}, 0, ErrBlobTooLarge
	}
	revision++
	if err := s.persistBlobLocked(context.Background(), userID, raw, revision); err != nil {
		return models.Blob{}, 0, err
	}
	return merged, revision, nil
}

func (s *Store) getLocked(userID string) (models.Blob, int64, error) {
	row := s.db.QueryRow(`SELECT json_blob, revision FROM user_blobs WHERE user_id = ?`, userID)
	var raw []byte
	var revision int64
	if err := row.Scan(&raw, &revision); err != nil {
		if err == sql.ErrNoRows {
			return models.EmptyBlob(), 0, nil
		}
		return models.Blob{}, 0, err
	}
	blob, err := ParseBlob(raw)
	if err != nil {
		return models.Blob{}, 0, err
	}
	return blob, revision, nil
}

// UpsertProgress updates one progress entry for a user.
func (s *Store) UpsertProgress(userID string, entry models.ProgressEntry) (models.Blob, int64, error) {
	incoming := models.Blob{Progress: map[string]models.ProgressEntry{entry.ID: entry}}
	return s.Put(userID, incoming)
}

// MarkWatched sets watched state on an existing progress entry.
func (s *Store) MarkWatched(userID, itemID string, watched bool) (models.Blob, int64, error) {
	blob, _, err := s.Get(userID)
	if err != nil {
		return models.Blob{}, 0, err
	}
	cur, ok := blob.Progress[itemID]
	if !ok {
		return models.Blob{}, 0, fmt.Errorf("progress entry not found")
	}
	cur.Watched = &watched
	if watched {
		cur.PositionSec = 0
	}
	return s.UpsertProgress(userID, cur)
}

// ToggleFavorite adds or removes a favorite entry.
func (s *Store) ToggleFavorite(userID string, entry models.FavoriteEntry) (models.Blob, int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	blob, revision, err := s.getLocked(userID)
	if err != nil {
		return models.Blob{}, 0, false, err
	}
	if _, ok := blob.Favorites[entry.ID]; ok {
		delete(blob.Favorites, entry.ID)
		merged, rev, putErr := s.saveLocked(userID, blob, revision)
		return merged, rev, false, putErr
	}
	blob.Favorites[entry.ID] = entry
	merged, rev, err := s.saveLocked(userID, blob, revision)
	return merged, rev, true, err
}

func (s *Store) saveLocked(userID string, blob models.Blob, revision int64) (models.Blob, int64, error) {
	raw, err := json.Marshal(blob)
	if err != nil {
		return models.Blob{}, 0, err
	}
	revision++
	if err := s.persistBlobLocked(context.Background(), userID, raw, revision); err != nil {
		return models.Blob{}, 0, err
	}
	return blob, revision, nil
}

// ListContinueWatching returns filtered progress entries for a user.
func (s *Store) ListContinueWatching(userID string, limit int) ([]models.ProgressEntry, error) {
	blob, _, err := s.Get(userID)
	if err != nil {
		return nil, err
	}
	return ContinueWatching(blob.Progress, limit), nil
}
