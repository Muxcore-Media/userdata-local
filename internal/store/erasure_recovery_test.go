package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muxcore-Media/userdata-local/internal/models"
	"github.com/Muxcore-Media/userdata-local/parental"
)

func seedLegacyBlob(t *testing.T, db *sql.DB, key string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO user_blobs (user_id, json_blob, revision, updated_at) VALUES (?, ?, 7, ?)`,
		key, `{"favorites":{"private":{"id":"private"}}}`, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

func rawBlobKeys(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT user_id FROM user_blobs ORDER BY user_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return keys
}

func requireRawBlobKeys(t *testing.T, db *sql.DB, want []string) {
	t.Helper()
	want = append([]string(nil), want...)
	sort.Strings(want)
	if got := rawBlobKeys(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("raw blob keys = %q, want %q", got, want)
	}
}

func appliedRecord(t *testing.T, db *sql.DB, erasureID string) [5]string {
	t.Helper()
	var record [5]string
	if err := db.QueryRow(`SELECT erasure_id, user_id, tenant_id, applied_at, counts_json FROM erasure_applied WHERE erasure_id = ?`, erasureID).
		Scan(&record[0], &record[1], &record[2], &record[3], &record[4]); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestBlobWritesRejectPaddedIDs(t *testing.T) {
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	for _, key := range []string{"", " victim", "victim ", "\tvictim\n", "\u2003victim\u00a0", "\u2028victim\u3000"} {
		_, _, err := st.Put(key, models.EmptyBlob())
		if !errors.Is(err, ErrInvalidUserID) {
			t.Errorf("Put(%q) = %v", key, err)
		}
		_, _, err = st.UpsertProgress(key, models.ProgressEntry{ID: "m"})
		if !errors.Is(err, ErrInvalidUserID) {
			t.Errorf("UpsertProgress(%q) = %v", key, err)
		}
		_, _, _, err = st.ToggleFavorite(key, models.FavoriteEntry{ID: "f"})
		if !errors.Is(err, ErrInvalidUserID) {
			t.Errorf("ToggleFavorite(%q) = %v", key, err)
		}
		// Cover the shared persistence boundary as well as public entry points.
		st.mu.Lock()
		err = st.persistBlobLocked(context.Background(), key, []byte(`{}`), 1)
		st.mu.Unlock()
		if !errors.Is(err, ErrInvalidUserID) {
			t.Errorf("shared persistence(%q) = %v", key, err)
		}
	}
	requireRawBlobKeys(t, st.db, nil)
	canonical := []string{"victim", "Victim", "victim-next", "victim~p~child", "victim smith", "victim\u200b"}
	for _, key := range canonical {
		if _, _, err := st.Put(key, models.EmptyBlob()); err != nil {
			t.Fatalf("canonical Put(%q): %v", key, err)
		}
	}
	requireRawBlobKeys(t, st.db, canonical)
}

func TestErasureCountsLegacyBlobAliasesAndPreservesUnrelatedKeys(t *testing.T) {
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	victim := []string{"victim", " victim ", "\tvictim\n", "\u2003victim\u00a0", "\u2028victim\u3000"}
	bystanders := []string{"Victim", "victim-next", "victim~p~child", "prefixvictim", "victim\u200b", " bystander "}
	for _, key := range append(append([]string(nil), victim...), bystanders...) {
		seedLegacyBlob(t, st.db, key)
	}
	ctx := context.Background()
	if n, err := st.CountUserRows(ctx, "victim"); err != nil || n != len(victim) {
		t.Fatalf("precondition = %d, %v", n, err)
	}
	counts, err := st.EraseUser(ctx, "er-victim", "victim", "home")
	if err != nil || counts[CountUserBlobs] != int64(len(victim)) {
		t.Fatalf("erase counts = %v, %v", counts, err)
	}
	if n, err := st.CountUserRows(ctx, "victim"); err != nil || n != 0 {
		t.Fatalf("postcondition = %d, %v", n, err)
	}
	requireRawBlobKeys(t, st.db, bystanders)
	record := appliedRecord(t, st.db, "er-victim")
	again, err := st.EraseUser(ctx, "er-victim", "victim", "home")
	if err != nil || !reflect.DeepEqual(counts, again) || record != appliedRecord(t, st.db, "er-victim") {
		t.Fatalf("replay changed historical counts/record: %v %v", again, err)
	}
	requireRawBlobKeys(t, st.db, bystanders)
}

func TestStartupRepairsAppliedBlobAliasesWithoutChangingHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "userdata.db")
	st := policyStore(t, path)
	// The original owner already acknowledged this canonical-only count. Use
	// deliberately noncanonical JSON formatting to detect even a re-encoding.
	const counts = `{ "user_blobs" : 1, "parental_policies" : 0 }`
	if _, err := st.db.Exec(`INSERT INTO erasure_applied VALUES (?, ?, ?, ?, ?)`,
		"er-victim", "victim", "home", "2026-02-03T04:05:06.123456789Z", counts); err != nil {
		t.Fatal(err)
	}
	before := appliedRecord(t, st.db, "er-victim")
	bystanders := []string{"Victim", "victim-next", "victim~p~child", "victim\u200b", " bystander "}
	for _, key := range append([]string{" victim ", "\tvictim\n", "\u2003victim\u00a0"}, bystanders...) {
		seedLegacyBlob(t, st.db, key)
	}
	// Scope is only legacy blobs: even a historical padded policy row is not
	// reinterpreted as a different account by this repair.
	if _, err := st.db.Exec(`INSERT INTO parental_policies VALUES (?, ?, ?, ?, ?, ?)`,
		"home", " victim ", `{"version":1,"mode":"unrestricted"}`, 1, "parent", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		st = policyStore(t, path)
		requireRawBlobKeys(t, st.db, bystanders)
		if got := appliedRecord(t, st.db, "er-victim"); got != before {
			t.Fatalf("startup changed applied record: got=%q before=%q", got, before)
		}
		var n int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM parental_policies WHERE user_id = ' victim '`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("startup changed excluded policy alias: %d %v", n, err)
		}
		if replay, err := st.EraseUser(context.Background(), "er-victim", "victim", "home"); err != nil || replay[CountUserBlobs] != 1 {
			t.Fatalf("historical replay: %v %v", replay, err)
		}
		if got := appliedRecord(t, st.db, "er-victim"); got != before {
			t.Fatalf("replay changed original JSON/timestamp: got=%q before=%q", got, before)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartupBlobRepairRollsBackEarlierDeletesAndRefusesStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "userdata.db")
	st := policyStore(t, path)
	if _, err := st.db.Exec(`INSERT INTO erasure_applied VALUES ('er-victim', 'victim', 'home', '2026-01-01T00:00:00Z', '{ "user_blobs" : 1 }')`); err != nil {
		t.Fatal(err)
	}
	record := appliedRecord(t, st.db, "er-victim")
	// ORDER BY user_id visits the ASCII-space alias before the Unicode alias.
	// The trigger refuses the later deletion after the first has succeeded.
	keys := []string{" victim ", "\u2003victim\u00a0", "bystander"}
	for _, key := range keys {
		seedLegacyBlob(t, st.db, key)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER fail_later_alias BEFORE DELETE ON user_blobs
		WHEN OLD.user_id = char(8195) || 'victim' || char(160)
		BEGIN SELECT RAISE(ABORT, 'later alias failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	failed, err := New(path)
	if failed != nil {
		_ = failed.Close()
		t.Fatal("startup returned a store after incomplete repair")
	}
	if err == nil {
		t.Fatal("startup ignored the injected repair failure")
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	requireRawBlobKeys(t, raw, keys)
	if got := appliedRecord(t, raw, "er-victim"); got != record {
		t.Fatalf("failed repair changed history: got=%q before=%q", got, record)
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_later_alias`); err != nil {
		t.Fatal(err)
	}
	st = policyStore(t, path)
	requireRawBlobKeys(t, st.db, []string{"bystander"})
	if got := appliedRecord(t, st.db, "er-victim"); got != record {
		t.Fatalf("successful retry changed history: got=%q before=%q", got, record)
	}
}

func awaitErasureSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for erasure test barrier")
	}
}

func awaitErasureResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for serialized write/erasure")
		return nil
	}
}

func TestErasureSerializesOverlappingBlobAndPolicyWrites(t *testing.T) {
	for _, kind := range []string{"blob", "policy-target", "policy-actor"} {
		for _, seen := range []bool{false, true} {
			name := kind + "/write-first"
			if seen {
				name = kind + "/seen-before-write"
			}
			t.Run(name, func(t *testing.T) {
				st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
				ctx := context.Background()
				if _, _, err := st.Put("victim", models.EmptyBlob()); err != nil {
					t.Fatal(err)
				}
				for _, user := range []string{"victim", "kid"} {
					if _, err := st.PutParentalPolicy(ctx, parental.Scope{UserID: user}, "parent", unrestrictedUpdate(0)); err != nil {
						t.Fatal(err)
					}
				}
				entered, release := make(chan struct{}), make(chan struct{})
				var enteredOnce, releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(unblock)
				var observed atomic.Bool
				st.SetErasureLookup(func(userID string) bool {
					if userID != "victim" {
						return false
					}
					enteredOnce.Do(func() { close(entered) })
					<-release
					return observed.Load()
				})
				written := make(chan error, 1)
				go func() {
					var err error
					switch kind {
					case "blob":
						_, _, err = st.Put("victim", models.Blob{Favorites: map[string]models.FavoriteEntry{"late": {ID: "late"}}})
					case "policy-target":
						_, err = st.PutParentalPolicy(ctx, parental.Scope{UserID: "victim"}, "parent", unrestrictedUpdate(1))
					case "policy-actor":
						_, err = st.PutParentalPolicy(ctx, parental.Scope{UserID: "kid"}, "victim", unrestrictedUpdate(1))
					}
					written <- err
				}()
				awaitErasureSignal(t, entered)
				if st.mu.TryLock() {
					st.mu.Unlock()
					t.Fatal("write did not retain the store lock during its last-seen check")
				}
				observed.Store(seen)
				eraseStarted, erased := make(chan struct{}), make(chan error, 1)
				go func() {
					close(eraseStarted)
					_, err := st.EraseUser(ctx, "er-victim", "victim", "")
					erased <- err
				}()
				awaitErasureSignal(t, eraseStarted)
				unblock()
				if err := awaitErasureResult(t, written); (seen && !errors.Is(err, ErrUserErased)) || (!seen && err != nil) {
					t.Fatalf("overlapping write, seen=%v: %v", seen, err)
				}
				if err := awaitErasureResult(t, erased); err != nil {
					t.Fatalf("overlapping erasure: %v", err)
				}
				if n, err := st.CountUserRows(ctx, "victim"); err != nil || n != 0 {
					t.Fatalf("post-erasure references: %d %v", n, err)
				}
				if _, _, err := st.Put("victim", models.EmptyBlob()); !errors.Is(err, ErrUserErased) {
					t.Fatalf("late write after serialized erasure: %v", err)
				}
				if _, _, err := st.Put("bystander", models.EmptyBlob()); err != nil {
					t.Fatalf("bystander write: %v", err)
				}
			})
		}
	}
}

func TestErasureLookupReplacementWaitsForActiveCheck(t *testing.T) {
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	st.SetErasureLookup(func(string) bool {
		close(entered)
		<-release
		return false
	})
	written := make(chan error, 1)
	go func() { _, _, err := st.Put("victim", models.EmptyBlob()); written <- err }()
	awaitErasureSignal(t, entered)
	replacing, replaced := make(chan struct{}), make(chan struct{})
	go func() {
		close(replacing)
		st.SetErasureLookup(func(string) bool { return true })
		close(replaced)
	}()
	awaitErasureSignal(t, replacing)
	if st.mu.TryLock() {
		st.mu.Unlock()
		t.Fatal("lookup invocation did not hold the lock protecting replacement")
	}
	unblock()
	if err := awaitErasureResult(t, written); err != nil {
		t.Fatal(err)
	}
	awaitErasureSignal(t, replaced)
	if _, _, err := st.Put("victim", models.EmptyBlob()); !errors.Is(err, ErrUserErased) {
		t.Fatalf("new lookup not installed: %v", err)
	}
}
