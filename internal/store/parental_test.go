package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Muxcore-Media/core/sdk/go/module/moduletest"
	"github.com/Muxcore-Media/userdata-local/internal/models"
	"github.com/Muxcore-Media/userdata-local/parental"
)

func policyStore(t *testing.T, path string) *Store {
	t.Helper()
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func unrestrictedUpdate(revision int64) parental.Update {
	return parental.Update{ExpectedRevision: revision, Policy: parental.Policy{Version: 1, Mode: "unrestricted"}}
}

func TestParentalPersistenceIsolationAndLegacyIndependence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "userdata.db")
	st := policyStore(t, path)
	scope := parental.Scope{UserID: "kid", TenantID: "home-a"}
	legacy := models.Blob{Prefs: &models.UserPreferences{Parental: json.RawMessage(`{"kids_mode":false,"pin_hash":"legacy-secret"}`)}}
	if _, _, err := st.Put(scope.UserID, legacy); err != nil {
		t.Fatal(err)
	}
	missing, err := st.GetParentalPolicy(ctx, scope)
	if err != nil || missing.State != "unconfigured" || missing.Policy != nil || missing.Revision != 0 {
		t.Fatalf("legacy blob promoted to authority: %+v %v", missing, err)
	}
	created, err := st.PutParentalPolicy(ctx, scope, "parent", unrestrictedUpdate(0))
	if err != nil || created.State != "configured" || created.Revision != 1 || created.Policy.Mode != "unrestricted" {
		t.Fatalf("create: %+v %v", created, err)
	}
	legacy.Prefs.Parental = json.RawMessage(`{"kids_mode":true,"max_parental_rating":"R"}`)
	if _, _, err := st.Put(scope.UserID, legacy); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = policyStore(t, path)
	got, err := st.GetParentalPolicy(ctx, scope)
	if err != nil || got.Policy == nil || got.Policy.Mode != "unrestricted" || got.Revision != 1 || got.UpdatedAt != created.UpdatedAt {
		t.Fatalf("restart/userdata overwrite: %+v %v", got, err)
	}
	other, err := st.GetParentalPolicy(ctx, parental.Scope{UserID: "kid", TenantID: "home-b"})
	if err != nil || other.State != "unconfigured" {
		t.Fatalf("cross-tenant row: %+v %v", other, err)
	}
	blob, revision, err := st.Get("kid")
	if err != nil || revision != 2 || string(blob.Prefs.Parental) != string(legacy.Prefs.Parental) {
		t.Fatalf("legacy data changed: %+v rev=%d err=%v", blob, revision, err)
	}
}

func TestParentalCASRejectsConcurrentAndStaleWriters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "userdata.db")
	st := policyStore(t, path)
	scope := parental.Scope{UserID: "kid"}
	for _, revision := range []int64{0, 1} {
		start := make(chan struct{})
		results := make(chan error, 12)
		var wg sync.WaitGroup
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := st.PutParentalPolicy(ctx, scope, "parent", unrestrictedUpdate(revision))
				results <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		writes, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				writes++
			case errors.Is(err, ErrPolicyConflict):
				conflicts++
			default:
				t.Fatalf("unexpected write error: %v", err)
			}
		}
		if writes != 1 || conflicts != 11 {
			t.Fatalf("revision %d writes=%d conflicts=%d", revision, writes, conflicts)
		}
	}
	other := policyStore(t, path)
	if _, err := other.PutParentalPolicy(ctx, scope, "parent", unrestrictedUpdate(1)); !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("second store accepted stale revision: %v", err)
	}
	if _, err := other.PutParentalPolicy(ctx, parental.Scope{UserID: "absent"}, "parent", unrestrictedUpdate(1)); !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("nonzero revision created absent row: %v", err)
	}
	got, err := other.GetParentalPolicy(ctx, scope)
	if err != nil || got.Revision != 2 {
		t.Fatalf("CAS final revision: %+v %v", got, err)
	}
}

func TestParentalStorageErrorsDoNotBecomeAbsence(t *testing.T) {
	ctx := context.Background()
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	scope := parental.Scope{UserID: "kid"}
	if _, err := st.PutParentalPolicy(ctx, scope, "parent", unrestrictedUpdate(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE parental_policies SET policy_json = ? WHERE user_id = ?`, []byte(`{"mode":"unrestricted"}`), scope.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetParentalPolicy(ctx, scope); err == nil {
		t.Fatal("corrupt policy read succeeded")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetParentalPolicy(ctx, scope); err == nil {
		t.Fatal("unavailable store read succeeded")
	}
	if _, err := st.PutParentalPolicy(ctx, scope, "parent", unrestrictedUpdate(1)); err == nil {
		t.Fatal("unavailable store write succeeded")
	}
}

func TestParentalMigrationLeavesLegacyFixtureUnconfigured(t *testing.T) {
	path := moduletest.CopyFixture(t, filepath.Join("testdata", "upgrade", "v0.1.0.db"))
	st := policyStore(t, path)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = policyStore(t, path)
	scope := parental.Scope{UserID: "alice@example.com"}
	policy, err := st.GetParentalPolicy(context.Background(), scope)
	if err != nil || policy.State != "unconfigured" || policy.Policy != nil {
		t.Fatalf("legacy fixture acquired authority: %+v %v", policy, err)
	}
	blob, revision, err := st.Get(scope.UserID)
	if err != nil || revision != 2 || blob.Progress["movie-1"].PositionSec != 120 || blob.Prefs == nil || string(blob.Prefs.Parental) != `{"kids_mode":true,"max_parental_rating":"PG","pin_hash":"abc123"}` {
		t.Fatalf("legacy fixture changed: rev=%d err=%v", revision, err)
	}
	moduletest.RequireIntegrity(t, st.db)
}
