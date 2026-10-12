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
	"sync"
	"testing"
	"time"

	"github.com/Muxcore-Media/userdata-local/conditional"
	"github.com/Muxcore-Media/userdata-local/internal/models"
)

func conditionalRead(t *testing.T, st *Store, user string) conditional.Document {
	t.Helper()
	d, err := st.GetConditional(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func conditionalUpdate(d conditional.Document, patch string) conditional.Update {
	return conditional.Update{Contract: conditional.Capability, RequestID: "request-1", Expected: d.Precondition(), Mutation: json.RawMessage(patch)}
}

func TestConditionalDurableRawCASAndNoop(t *testing.T) {
	ctx := context.Background()
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	initial := conditionalRead(t, st, "alice")
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
	current := conditionalRead(t, st, "alice")
	update := conditionalUpdate(current, `{"prefs":{"display":{"theme":"light"}},"favorites":{"remove":["x"]},"wantToWatch":{"set":{"y":{"id":"y"}}},"playlists":[],"queue":[]}`)
	ack, err := st.PutConditional(ctx, "alice", update)
	if err != nil || ack.Revision != "9007199254740994" || ack.PreviousRevision != current.Revision {
		t.Fatalf("ACK: %+v %v", ack, err)
	}
	read := conditionalRead(t, st, "alice")
	if read.Outcome != "" || read.RequestID != "" || !bytes.Equal(read.Blob, ack.Blob) || !bytes.Contains(read.Blob, []byte("9007199254740993")) || !bytes.Contains(read.Blob, []byte("1.234567890123456789e+99")) {
		t.Fatal("raw persistence/GET boundary")
	}
	if _, err := st.PutConditional(ctx, "alice", update); !errors.Is(err, conditional.RevisionConflict) {
		t.Fatalf("repeated request falsely deduplicated: %v", err)
	}
	after := conditionalRead(t, st, "alice")
	if after.Revision != read.Revision || !bytes.Equal(after.Blob, read.Blob) {
		t.Fatal("conflict changed state")
	}
	noop, err := st.PutConditional(ctx, "alice", conditionalUpdate(after, `{"favorites":{"remove":["absent"]}}`))
	if err != nil || noop.Revision != "9007199254740995" || !bytes.Equal(noop.Blob, after.Blob) {
		t.Fatalf("no-op must advance once: %+v %v", noop, err)
	}
	wrong := conditionalUpdate(noop, `{"queue":[]}`)
	wrong.Expected.UserID = "bob"
	if _, err := st.PutConditional(ctx, "alice", wrong); !errors.Is(err, conditional.InvalidPrecondition) {
		t.Fatalf("cross-account precondition: %v", err)
	}
}

func TestConditionalConcurrentConnectionsOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "userdata.db")
	a := policyStore(t, path)
	b := policyStore(t, path)
	for round := 0; round < 6; round++ {
		user := "user-" + strconv.Itoa(round)
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i, st := range []*Store{a, b} {
			d := conditionalRead(t, st, user)
			patch := `{"prefs":{"display":{"theme":"light"}}}`
			if i == 1 {
				patch = `{"favorites":{"set":{"x":{"id":"x"}}}}`
			}
			wg.Add(1)
			go func(st *Store, u conditional.Update) {
				defer wg.Done()
				<-start
				_, err := st.PutConditional(context.Background(), user, u)
				errs <- err
			}(st, conditionalUpdate(d, patch))
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
		if wins != 1 || conflicts != 1 || conditionalRead(t, a, user).Revision != "1" {
			t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
		}
	}
}

func TestConditionalBothWriteOrdersRefuseStaleIntent(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
		read := conditionalRead(t, st, "alice")
		patches := []string{`{"prefs":{"display":{"theme":"light"}}}`, `{"favorites":{"set":{"x":{"id":"x"}}}}`}
		if reverse {
			patches[0], patches[1] = patches[1], patches[0]
		}
		first, err := st.PutConditional(context.Background(), "alice", conditionalUpdate(read, patches[0]))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutConditional(context.Background(), "alice", conditionalUpdate(read, patches[1])); !errors.Is(err, conditional.RevisionConflict) {
			t.Fatal(err)
		}
		after := conditionalRead(t, st, "alice")
		if after.Revision != first.Revision || !bytes.Equal(after.Blob, first.Blob) {
			t.Fatal("stale disjoint write merged implicitly")
		}
	}
}

func TestConditionalReopenAndStoppedRestoreRotateAuthority(t *testing.T) {
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
	first, err := st.PutConditional(ctx, "alice", conditionalUpdate(conditionalRead(t, st, "alice"), `{"prefs":{"display":{"theme":"light"}}}`))
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
	second := conditionalRead(t, st, "alice")
	if second.Authority == first.Authority || second.Revision != first.Revision || !bytes.Equal(second.Blob, first.Blob) {
		t.Fatal("reopen continuity")
	}
	if _, err := st.PutConditional(ctx, "alice", conditionalUpdate(first, `{"queue":[]}`)); !errors.Is(err, conditional.AuthorityConflict) {
		t.Fatalf("old authority accepted: %v", err)
	}
	if _, err := st.PutConditional(ctx, "alice", conditionalUpdate(second, `{"prefs":{"display":{"theme":"dark"}}}`)); err != nil {
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
	restored := conditionalRead(t, st, "alice")
	if restored.Authority == second.Authority || restored.Revision != "1" || !bytes.Equal(restored.Blob, first.Blob) {
		t.Fatal("stopped restore")
	}
	if _, err := st.PutConditional(ctx, "alice", conditionalUpdate(second, `{"queue":[]}`)); !errors.Is(err, conditional.AuthorityConflict) {
		t.Fatalf("restored revision reused old authority: %v", err)
	}
}

func TestConditionalErasureOverflowAndLegacyInterference(t *testing.T) {
	ctx := context.Background()
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	seen := conditionalRead(t, st, "seen")
	st.SetErasureLookup(func(id string) bool { return id == "seen" })
	if _, err := st.PutConditional(ctx, "seen", conditionalUpdate(seen, `{"queue":[]}`)); !errors.Is(err, ErrUserErased) {
		t.Fatalf("seen erasure: %v", err)
	}
	if _, err := st.EraseUser(ctx, "fixture-erasure", "erased", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutConditional(ctx, "erased", conditionalUpdate(conditionalRead(t, st, "erased"), `{"queue":[]}`)); !errors.Is(err, ErrUserErased) {
		t.Fatalf("durable erasure: %v", err)
	}
	if _, err := st.db.Exec("INSERT INTO user_blobs(user_id,json_blob,revision) VALUES (?,?,?)", "full", []byte(`{}`), int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	full := conditionalRead(t, st, "full")
	if _, err := st.PutConditional(ctx, "full", conditionalUpdate(full, `{"queue":[]}`)); !errors.Is(err, conditional.RevisionExhausted) {
		t.Fatalf("overflow: %v", err)
	}
	if after := conditionalRead(t, st, "full"); after.Revision != full.Revision || string(after.Blob) != "{}" {
		t.Fatal("overflow changed row")
	}
	first, err := st.PutConditional(ctx, "alice", conditionalUpdate(conditionalRead(t, st, "alice"), `{"wantToWatch":{"set":{"x":{"id":"x"}}},"favorites":{"remove":["x"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Put("alice", models.Blob{Favorites: map[string]models.FavoriteEntry{"x": {ID: "x"}}}); err != nil {
		t.Fatal(err)
	}
	legacy := conditionalRead(t, st, "alice")
	if !bytes.Contains(legacy.Blob, []byte(`"x"`)) || bytes.Contains(legacy.Blob, []byte("wantToWatch")) {
		t.Fatal("legacy limitation no longer described by fixture")
	}
	if _, err := st.PutConditional(ctx, "alice", conditionalUpdate(first, `{"queue":[]}`)); !errors.Is(err, conditional.RevisionConflict) {
		t.Fatalf("legacy revision ignored: %v", err)
	}
}

func TestConditionalCanceledContentionReleasesConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "userdata.db")
	st := policyStore(t, path)
	other := policyStore(t, path)
	read := conditionalRead(t, st, "alice")
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
	_, err = st.PutConditional(ctx, "alice", conditionalUpdate(read, `{"queue":[]}`))
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
	if _, err := st.PutConditional(recovery, "alice", conditionalUpdate(read, `{"queue":[]}`)); err != nil {
		t.Fatalf("canceled transaction poisoned pool: %v", err)
	}
}

func TestConditionalAndErasureOverlapInBothOrders(t *testing.T) {
	for _, eraseFirst := range []bool{false, true} {
		name := "write first"
		if eraseFirst {
			name = "erase first"
		}
		t.Run(name, func(t *testing.T) {
			st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			seed, err := st.PutConditional(ctx, "alice", conditionalUpdate(conditionalRead(t, st, "alice"), `{"queue":[]}`))
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
				_, err := st.PutConditional(ctx, "alice", conditionalUpdate(seed, `{"favorites":{"set":{"late":{"id":"late"}}}}`))
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
