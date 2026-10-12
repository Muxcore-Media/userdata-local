package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muxcore-Media/userdata-local/conditional"
	"github.com/Muxcore-Media/userdata-local/internal/models"
)

func conditionalReadV3(t *testing.T, st *Store, user string) conditional.Document {
	t.Helper()
	d, err := st.GetConditionalV3(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func conditionalUpdateV3(d conditional.Document, patch string) conditional.Update {
	return conditional.Update{Contract: conditional.CapabilityV3, RequestID: "request-1", Expected: d.Precondition(), Mutation: json.RawMessage(patch)}
}

func TestConditionalV3DurableRawCASAndNoop(t *testing.T) {
	ctx := context.Background()
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	initial := conditionalReadV3(t, st, "alice")
	if initial.Exists || initial.Revision != "0" {
		t.Fatal("missing record manufactured")
	}
	var count int
	if err := st.db.QueryRow("SELECT count(*) FROM user_blobs").Scan(&count); err != nil || count != 0 {
		t.Fatalf("GET created a row: %d %v", count, err)
	}
	if _, err := st.db.Exec("INSERT INTO user_blobs(user_id,json_blob,revision) VALUES (?,?,?)", "alice", []byte(`{"progress":{"x":{"future":9007199254740993}},"unknown":1.234567890123456789e+99,"favorites":{"x":{"id":"x"}}}`), int64(9007199254740993)); err != nil {
		t.Fatal(err)
	}
	current := conditionalReadV3(t, st, "alice")
	update := conditionalUpdateV3(current, `{"prefs":{"display":{"theme":"light"}},"favorites":{"remove":["x"]},"wantToWatch":{"set":{"y":{"id":"y"}}},"playlists":[],"queue":[]}`)
	ack, err := st.PutConditionalV3(ctx, "alice", update)
	if err != nil || ack.Revision != "9007199254740994" || ack.PreviousRevision != current.Revision {
		t.Fatalf("ACK: %+v %v", ack, err)
	}
	read := conditionalReadV3(t, st, "alice")
	if read.Outcome != "" || read.RequestID != "" || !bytes.Equal(read.Blob, ack.Blob) || !bytes.Contains(read.Blob, []byte("9007199254740993")) || !bytes.Contains(read.Blob, []byte("1.234567890123456789e+99")) {
		t.Fatal("raw persistence/GET boundary")
	}
	if _, err := st.PutConditionalV3(ctx, "alice", update); !errors.Is(err, conditional.RevisionConflict) {
		t.Fatalf("repeated request falsely deduplicated: %v", err)
	}
	after := conditionalReadV3(t, st, "alice")
	if after.Revision != read.Revision || !bytes.Equal(after.Blob, read.Blob) {
		t.Fatal("conflict changed state")
	}
	noop, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(after, `{"favorites":{"remove":["absent"]}}`))
	if err != nil || noop.Revision != "9007199254740995" || !bytes.Equal(noop.Blob, after.Blob) {
		t.Fatalf("no-op must advance once: %+v %v", noop, err)
	}
	wrong := conditionalUpdateV3(noop, `{"queue":[]}`)
	wrong.Expected.UserID = "bob"
	if _, err := st.PutConditionalV3(ctx, "alice", wrong); !errors.Is(err, conditional.InvalidPrecondition) {
		t.Fatalf("cross-account precondition: %v", err)
	}
}

func TestConditionalV3ConcurrentConnectionsOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "userdata.db")
	a := policyStore(t, path)
	b := policyStore(t, path)
	for round := 0; round < 6; round++ {
		user := "user-" + strconv.Itoa(round)
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i, st := range []*Store{a, b} {
			d := conditionalReadV3(t, st, user)
			patch := `{"progress":{"report":{"x":{"positionSec":1,"durationSec":100}}}}`
			if i == 1 {
				patch = `{"progress":{"set_watched":{"x":true}}}`
			}
			wg.Add(1)
			go func(st *Store, u conditional.Update) {
				defer wg.Done()
				<-start
				_, err := st.PutConditionalV3(context.Background(), user, u)
				errs <- err
			}(st, conditionalUpdateV3(d, patch))
		}
		close(start)
		wg.Wait()
		close(errs)
		wins, conflicts := 0, 0
		for err := range errs {
			if err == nil {
				wins++
			} else if errors.Is(err, conditional.RevisionConflict) {
				conflicts++
			} else {
				t.Fatalf("unexpected contention error: %v", err)
			}
		}
		if wins != 1 || conflicts != 1 || conditionalReadV3(t, a, user).Revision != "1" {
			t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
		}
	}
}

func TestConditionalV3BothWriteOrdersRefuseStaleIntent(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
		read := conditionalReadV3(t, st, "alice")
		patches := []string{`{"progress":{"report":{"x":{"positionSec":12.500,"durationSec":100,"observedAt":"2026-10-12T10:00:00+01:00"}}}}`, `{"favorites":{"set":{"x":{"id":"x"}}}}`}
		if reverse {
			patches[0], patches[1] = patches[1], patches[0]
		}
		first, err := st.PutConditionalV3(context.Background(), "alice", conditionalUpdateV3(read, patches[0]))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutConditionalV3(context.Background(), "alice", conditionalUpdateV3(read, patches[1])); !errors.Is(err, conditional.RevisionConflict) {
			t.Fatal(err)
		}
		after := conditionalReadV3(t, st, "alice")
		if after.Revision != first.Revision || !bytes.Equal(after.Blob, first.Blob) {
			t.Fatal("stale disjoint write merged implicitly")
		}
	}
}

func TestConditionalV3ReopenAndStoppedRestoreRotateAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "userdata.db")
	open := func() *Store {
		st, err := New(path)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st := open()
	first, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(conditionalReadV3(t, st, "alice"), `{"progress":{"report":{"x":{"positionSec":12.500,"durationSec":100,"observedAt":"2026-10-12T10:00:00+01:00"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st = open()
	second := conditionalReadV3(t, st, "alice")
	if second.Authority == first.Authority || second.Revision != first.Revision || !bytes.Equal(second.Blob, first.Blob) {
		t.Fatal("reopen continuity")
	}
	if _, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(first, `{"queue":[]}`)); !errors.Is(err, conditional.AuthorityConflict) {
		t.Fatalf("old authority accepted: %v", err)
	}
	if _, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(second, `{"progress":{"set_watched":{"x":true}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	st = open()
	defer st.Close()
	restored := conditionalReadV3(t, st, "alice")
	if restored.Authority == second.Authority || restored.Revision != "1" || !bytes.Equal(restored.Blob, first.Blob) {
		t.Fatal("stopped restore")
	}
	if _, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(second, `{"queue":[]}`)); !errors.Is(err, conditional.AuthorityConflict) {
		t.Fatalf("restored revision reused old authority: %v", err)
	}
}

func TestConditionalV3ErasureOverflowAndLegacyInterference(t *testing.T) {
	ctx := context.Background()
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	seen := conditionalReadV3(t, st, "seen")
	st.SetErasureLookup(func(id string) bool { return id == "seen" })
	if _, err := st.PutConditionalV3(ctx, "seen", conditionalUpdateV3(seen, `{"queue":[]}`)); !errors.Is(err, ErrUserErased) {
		t.Fatalf("seen erasure: %v", err)
	}
	if _, err := st.EraseUser(ctx, "fixture-erasure", "erased", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutConditionalV3(ctx, "erased", conditionalUpdateV3(conditionalReadV3(t, st, "erased"), `{"queue":[]}`)); !errors.Is(err, ErrUserErased) {
		t.Fatalf("durable erasure: %v", err)
	}
	if _, err := st.db.Exec("INSERT INTO user_blobs(user_id,json_blob,revision) VALUES (?,?,?)", "full", []byte(`{}`), int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	full := conditionalReadV3(t, st, "full")
	if _, err := st.PutConditionalV3(ctx, "full", conditionalUpdateV3(full, `{"queue":[]}`)); !errors.Is(err, conditional.RevisionExhausted) {
		t.Fatalf("overflow: %v", err)
	}
	if after := conditionalReadV3(t, st, "full"); after.Revision != full.Revision || string(after.Blob) != "{}" {
		t.Fatal("overflow changed row")
	}
	first, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(conditionalReadV3(t, st, "alice"), `{"wantToWatch":{"set":{"x":{"id":"x"}}},"favorites":{"remove":["x"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Put("alice", models.Blob{Favorites: map[string]models.FavoriteEntry{"x": {ID: "x"}}}); err != nil {
		t.Fatal(err)
	}
	legacy := conditionalReadV3(t, st, "alice")
	if !bytes.Contains(legacy.Blob, []byte(`"x"`)) || bytes.Contains(legacy.Blob, []byte("wantToWatch")) {
		t.Fatal("legacy limitation no longer described by fixture")
	}
	if _, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(first, `{"queue":[]}`)); !errors.Is(err, conditional.RevisionConflict) {
		t.Fatalf("legacy revision ignored: %v", err)
	}
}

func TestConditionalV3CanceledContentionReleasesConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "userdata.db")
	st := policyStore(t, path)
	other := policyStore(t, path)
	read := conditionalReadV3(t, st, "alice")
	conn, err := other.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(read, `{"queue":[]}`))
	if err == nil {
		t.Fatal("contended canceled write succeeded")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("busy handler ignored short request deadline: %s", elapsed)
	}
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	recovery, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := st.PutConditionalV3(recovery, "alice", conditionalUpdateV3(read, `{"queue":[]}`)); err != nil {
		t.Fatalf("canceled transaction poisoned pool: %v", err)
	}
}

func TestConditionalV3AndErasureOverlapInBothOrders(t *testing.T) {
	for _, eraseFirst := range []bool{false, true} {
		name := "write first"
		if eraseFirst {
			name = "erase first"
		}
		t.Run(name, func(t *testing.T) {
			st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			seed, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(conditionalReadV3(t, st, "alice"), `{"queue":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			// Hold the only database connection. The first real API acquires
			// the Store mutex and then waits for this lease; the second real
			// API overlaps it and must serialize behind that mutex.
			lease, err := st.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var workers sync.WaitGroup
			defer func() {
				cancel()
				_ = lease.Close()
				done := make(chan struct{})
				go func() {
					workers.Wait()
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("overlap workers did not stop after cancellation and lease release")
				}
			}()
			waited := st.db.Stats().WaitCount
			writeResult := make(chan error, 1)
			type eraseResult struct {
				counts map[string]int64
				err    error
			}
			erased := make(chan eraseResult, 1)
			write := func() {
				_, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(seed, `{"progress":{"set_watched":{"late":true}}}`))
				writeResult <- err
			}
			erase := func() {
				counts, err := st.EraseUser(ctx, "overlap", "alice", "")
				erased <- eraseResult{counts: counts, err: err}
			}
			first, second := write, erase
			if eraseFirst {
				first, second = erase, write
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				first()
			}()
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for st.db.Stats().WaitCount == waited {
				select {
				case <-tick.C:
				case <-deadline.C:
					t.Fatal("first API never waited for the held connection")
				}
			}
			started := make(chan struct{})
			workers.Add(1)
			go func() {
				defer workers.Done()
				close(started)
				second()
			}()
			<-started
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			var writeErr error
			var eraseOutcome eraseResult
			select {
			case writeErr = <-writeResult:
			case <-ctx.Done():
				t.Fatal("overlapping write did not finish", ctx.Err())
			}
			select {
			case eraseOutcome = <-erased:
			case <-ctx.Done():
				t.Fatal("overlapping erase did not finish", ctx.Err())
			}
			if eraseOutcome.err != nil || eraseOutcome.counts[CountUserBlobs] != 1 {
				t.Fatalf("actual erasure failed: %+v", eraseOutcome)
			}
			if eraseFirst && !errors.Is(writeErr, ErrUserErased) {
				t.Fatalf("post-erasure write escaped refusal: %v", writeErr)
			}
			if !eraseFirst && writeErr != nil {
				t.Fatalf("pre-erasure write failed: %v", writeErr)
			}
			if rows, err := st.CountUserRows(ctx, "alice"); err != nil || rows != 0 {
				t.Fatalf("overlap resurrected user rows: %d %v", rows, err)
			}
			if applied, err := st.ErasureApplied(ctx, "overlap"); err != nil || !applied {
				t.Fatalf("missing durable erasure: %v %v", applied, err)
			}
		})
	}
}

func TestConditionalV3SharesAuthorityAndCASWithV2(t *testing.T) {
	ctx := context.Background()
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	v2 := conditionalRead(t, st, "alice")
	v3 := conditionalReadV3(t, st, "alice")
	if v2.Authority != v3.Authority || v2.Revision != v3.Revision || !bytes.Equal(v2.Blob, v3.Blob) {
		t.Fatal("versions have separate authority/data")
	}
	first, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(v3, `{"progress":{"report":{"x":{"positionSec":80,"durationSec":100,"observedAt":"2099-01-01T00:00:00Z"}},"y":{"positionSec":80,"durationSec":100}}}}`))
	// Use a valid report map; malformed mixed commands must leave the shared row absent.
	if !errors.Is(err, conditional.InvalidBody) || first.Revision != "" {
		t.Fatalf("bad mixed command: %v", err)
	}
	if conditionalRead(t, st, "alice").Revision != "0" {
		t.Fatal("invalid command wrote row")
	}
	first, err = st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(v3, `{"progress":{"report":{"x":{"positionSec":80,"durationSec":100,"observedAt":"2099-01-01T00:00:00Z"},"y":{"positionSec":10,"durationSec":100}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	_ = json.Unmarshal(first.Blob, &document)
	var entries map[string]map[string]json.RawMessage
	_ = json.Unmarshal(document["progress"], &entries)
	if !bytes.Equal(entries["x"]["updatedAt"], entries["y"]["updatedAt"]) {
		t.Fatal("receipt sampled separately")
	}
	var received string
	_ = json.Unmarshal(entries["x"]["updatedAt"], &received)
	if parsed, err := time.Parse("2006-01-02T15:04:05.000000000Z", received); err != nil || parsed.Location() != time.UTC {
		t.Fatalf("noncanonical receipt %q", received)
	}
	if _, err := st.PutConditional(ctx, "alice", conditionalUpdate(v2, `{"queue":[]}`)); !errors.Is(err, conditional.RevisionConflict) {
		t.Fatal("v2 bypassed v3 revision")
	}
	v2 = conditionalRead(t, st, "alice")
	if _, err := st.PutConditional(ctx, "alice", conditionalUpdate(v2, `{"prefs":{"display":{"theme":"light"}}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(first, `{"progress":{"remove":["x"]}}`)); !errors.Is(err, conditional.RevisionConflict) {
		t.Fatal("v3 bypassed v2 revision")
	}
	before := conditionalReadV3(t, st, "alice")
	older, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(before, `{"progress":{"report":{"x":{"positionSec":2.500,"durationSec":100,"observedAt":"1900-01-01T00:00:00Z"}},"set_watched":{"x":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if older.Revision != "3" || !bytes.Contains(older.Blob, []byte(`"positionSec":2.500`)) || !bytes.Contains(older.Blob, []byte(`"observedAt":"1900-01-01T00:00:00.000000000Z"`)) {
		t.Fatal("clock selected winner instead of current CAS")
	}
	if _, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(older, `{"prefs":{"display":{"theme":"dark"}},"progress":{"set_watched":{"x":null}}}`)); !errors.Is(err, conditional.InvalidBody) {
		t.Fatal(err)
	}
	after := conditionalReadV3(t, st, "alice")
	if after.Revision != older.Revision || !bytes.Equal(after.Blob, older.Blob) {
		t.Fatal("invalid mixed command partially committed")
	}
	noop, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(after, `{"progress":{"remove":["absent"]}}`))
	if err != nil || noop.Revision != "4" || !bytes.Equal(noop.Blob, after.Blob) {
		t.Fatal("remove no-op must increment exactly once")
	}
}

func TestConditionalV3StoredValidationAndSizeRollBackOtherFamilies(t *testing.T) {
	ctx := context.Background()
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	for i, blob := range []string{`{"progress":{"x":{"id":"wrong"}}}`, `{"progress":{"x":{"durationSec":"bad"}}}`} {
		user := strconv.Itoa(i)
		if _, err := st.db.Exec("INSERT INTO user_blobs(user_id,json_blob,revision) VALUES (?,?,1)", user, []byte(blob)); err != nil {
			t.Fatal(err)
		}
		before := conditionalReadV3(t, st, user)
		if _, err := st.PutConditionalV3(ctx, user, conditionalUpdateV3(before, `{"queue":[{"id":"other"}],"progress":{"set_watched":{"x":true}}}`)); !errors.Is(err, conditional.StorageUnavailable) {
			t.Fatal(err)
		}
		after := conditionalReadV3(t, st, user)
		if before.Revision != after.Revision || !bytes.Equal(before.Blob, after.Blob) {
			t.Fatal("storage refusal partially committed")
		}
	}
	huge, _ := conditional.Encode(map[string]any{"future": strings.Repeat("x", conditional.MaxBlobBytes-100)})
	if _, err := st.db.Exec("INSERT INTO user_blobs(user_id,json_blob,revision) VALUES (?,?,1)", "large", huge); err != nil {
		t.Fatal(err)
	}
	before := conditionalReadV3(t, st, "large")
	if _, err := st.PutConditionalV3(ctx, "large", conditionalUpdateV3(before, `{"queue":[],"progress":{"report":{"x":{"positionSec":1,"durationSec":2,"metadata":{"title":"long enough to exceed the exact final blob limit"}}}}}`)); !errors.Is(err, conditional.TooLarge) {
		t.Fatal(err)
	}
	after := conditionalReadV3(t, st, "large")
	if before.Revision != after.Revision || !bytes.Equal(before.Blob, after.Blob) {
		t.Fatal("oversize partially committed")
	}
}

func TestConditionalV3PersistenceFailureNeverAcknowledgesAndRollsBack(t *testing.T) {
	for _, operation := range []string{"INSERT", "UPDATE"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
			if operation == "UPDATE" {
				if _, err := st.PutConditionalV3(ctx, "alice", conditionalUpdateV3(conditionalReadV3(t, st, "alice"), `{"prefs":{"future":9007199254740993},"progress":{"report":{"x":{"positionSec":12.500,"durationSec":100}}}}`)); err != nil {
					t.Fatal(err)
				}
			}
			before := conditionalReadV3(t, st, "alice")
			// Test-only trigger in this isolated fixture DB: force the real SQL
			// persistence statement to fail after mutation and ACK encoding.
			if _, err := st.db.Exec("CREATE TEMP TRIGGER fail_v3_write BEFORE " + operation + " ON user_blobs BEGIN SELECT RAISE(ABORT, 'fixture write failure'); END"); err != nil {
				t.Fatal(err)
			}
			update := conditionalUpdateV3(before, `{"prefs":{"display":{"theme":"light"}},"queue":[],"progress":{"report":{"x":{"positionSec":1.250,"durationSec":100}},"set_watched":{"x":false}}}`)
			ack, err := st.PutConditionalV3(ctx, "alice", update)
			if err == nil || !strings.Contains(err.Error(), "fixture write failure") || ack.Contract != "" || ack.Outcome != "" || ack.Revision != "" || len(ack.Blob) != 0 {
				t.Fatalf("failed persistence acknowledged: %+v %v", ack, err)
			}
			after := conditionalReadV3(t, st, "alice")
			if after.Revision != before.Revision || after.Exists != before.Exists || !bytes.Equal(after.Blob, before.Blob) {
				t.Fatal("failed persistence changed row")
			}
			if _, err := st.db.Exec("DROP TRIGGER fail_v3_write"); err != nil {
				t.Fatal(err)
			}
			ack, err = st.PutConditionalV3(ctx, "alice", update)
			if err != nil || ack.PreviousRevision != before.Revision || ack.Outcome != "applied" {
				t.Fatalf("failed transaction poisoned original-precondition retry: %+v %v", ack, err)
			}
		})
	}
}

func TestConditionalV3CanceledAfterApplicationRollsBack(t *testing.T) {
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	before := conditionalReadV3(t, st, "alice")
	update := conditionalUpdateV3(before, `{"queue":[],"progress":{"set_watched":{"x":true}}}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	applied := false
	ack, err := st.putConditional(ctx, "alice", update, conditional.OperationsV3(), func(raw, patch json.RawMessage) (json.RawMessage, error) {
		result, err := conditional.ApplyV3(raw, patch, time.Now())
		applied = err == nil
		cancel()
		return result, err
	})
	if !applied || !errors.Is(err, context.Canceled) || ack.Outcome != "" || ack.Revision != "" {
		t.Fatalf("post-application cancellation acknowledged: %+v %v", ack, err)
	}
	after := conditionalReadV3(t, st, "alice")
	if after.Revision != before.Revision || !bytes.Equal(after.Blob, before.Blob) {
		t.Fatal("canceled transaction changed row")
	}
	if _, err := st.PutConditionalV3(context.Background(), "alice", update); err != nil {
		t.Fatalf("canceled transaction poisoned connection: %v", err)
	}
}
