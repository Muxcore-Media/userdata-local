package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Muxcore-Media/userdata-local/internal/models"
	"github.com/Muxcore-Media/userdata-local/parental"
)

func TestEraseUserDispositionAndBystanders(t *testing.T) {
	ctx := context.Background()
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	for _, u := range []string{"victim", "bystander", "other-tenant-user"} {
		if _, _, err := st.UpsertProgress(u, models.ProgressEntry{ID: "m1", Kind: models.MediaKindMovie, Title: "T", Href: "/m1", PositionSec: 1, DurationSec: 10, UpdatedAt: "2026-01-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	put := func(tenant, user, actor string) {
		t.Helper()
		if _, err := st.PutParentalPolicy(ctx, parental.Scope{TenantID: tenant, UserID: user}, actor, unrestrictedUpdate(0)); err != nil {
			t.Fatal(err)
		}
	}
	put("home", "victim", "parent")
	put("home", "bystander", "victim")
	put("home", "bystander2", "parent")
	put("elsewhere", "victim", "parent")
	put("elsewhere", "other-tenant-user", "other-admin")

	counts, err := st.EraseUser(ctx, "er-1", "victim", "home")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{CountUserBlobs: 1, CountParentalPolicies: 1, CountParentalPoliciesOtherTenant: 1, CountParentalPoliciesAnonymise: 1}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("counts[%s] = %d, want %d (all: %v)", k, counts[k], v, counts)
		}
	}
	if n, err := st.CountUserRows(ctx, "victim"); err != nil || n != 0 {
		t.Fatalf("post-condition = %d, %v", n, err)
	}
	if ok, err := st.ErasureApplied(ctx, "er-1"); err != nil || !ok {
		t.Fatalf("applied = %v, %v", ok, err)
	}
	if ok, err := st.ErasureApplied(ctx, "er-2"); err != nil || ok {
		t.Fatalf("unrelated erasure applied = %v, %v", ok, err)
	}

	// Bystanders: blobs, same-tenant policies and other-tenant policies intact.
	for _, u := range []string{"bystander", "other-tenant-user"} {
		if _, rev, err := st.Get(u); err != nil || rev != 1 {
			t.Errorf("bystander blob %s rev=%d err=%v", u, rev, err)
		}
	}
	var by string
	var rev int64
	if err := st.db.QueryRow(`SELECT updated_by, revision FROM parental_policies WHERE tenant_id='home' AND user_id='bystander'`).Scan(&by, &rev); err != nil {
		t.Fatalf("bystander policy deleted: %v", err)
	}
	if by != AnonymisedActor || rev != 1 {
		t.Errorf("bystander policy updated_by=%q revision=%d, want %q and 1", by, rev, AnonymisedActor)
	}
	for _, k := range [][2]string{{"home", "bystander2"}, {"elsewhere", "other-tenant-user"}} {
		var n int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM parental_policies WHERE tenant_id=? AND user_id=?`, k[0], k[1]).Scan(&n); err != nil || n != 1 {
			t.Errorf("bystander policy %v count=%d err=%v", k, n, err)
		}
	}
	if err := st.db.QueryRow(`SELECT updated_by FROM parental_policies WHERE user_id='other-tenant-user'`).Scan(&by); err != nil || by != "other-admin" {
		t.Errorf("other-tenant policy updated_by=%q err=%v", by, err)
	}

	// Applying the same erasure again changes nothing and returns the record.
	again, err := st.EraseUser(ctx, "er-1", "victim", "home")
	if err != nil || again[CountUserBlobs] != 1 {
		t.Fatalf("second apply: %v %v", again, err)
	}
	var applied int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM erasure_applied`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("erasure_applied rows = %d, %v", applied, err)
	}
}

func TestEraseUserDifferentUserSameTenantUntouched(t *testing.T) {
	ctx := context.Background()
	st := policyStore(t, filepath.Join(t.TempDir(), "userdata.db"))
	if _, err := st.PutParentalPolicy(ctx, parental.Scope{TenantID: "home", UserID: "a"}, "parent", unrestrictedUpdate(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutParentalPolicy(ctx, parental.Scope{TenantID: "home", UserID: "b"}, "parent", unrestrictedUpdate(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EraseUser(ctx, "er-a", "a", "home"); err != nil {
		t.Fatal(err)
	}
	if d, err := st.GetParentalPolicy(ctx, parental.Scope{TenantID: "home", UserID: "b"}); err != nil || d.State != "configured" {
		t.Fatalf("b = %+v %v", d, err)
	}
	if d, err := st.GetParentalPolicy(ctx, parental.Scope{TenantID: "home", UserID: "a"}); err != nil || d.State != "unconfigured" {
		t.Fatalf("a = %+v %v", d, err)
	}
}

func TestEraseUserRollsBackOnMidTransactionFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "userdata.db")
	st := policyStore(t, path)
	if _, _, err := st.UpsertProgress("victim", models.ProgressEntry{ID: "m1", Kind: models.MediaKindMovie, Title: "T", Href: "/m1", UpdatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutParentalPolicy(ctx, parental.Scope{TenantID: "home", UserID: "victim"}, "parent", unrestrictedUpdate(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutParentalPolicy(ctx, parental.Scope{TenantID: "home", UserID: "kid"}, "victim", unrestrictedUpdate(0)); err != nil {
		t.Fatal(err)
	}
	// The deletes and the anonymisation succeed; the final applied-record
	// insert is aborted by a trigger, so everything must roll back.
	if _, err := st.db.Exec(`CREATE TRIGGER inject_failure BEFORE INSERT ON erasure_applied
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EraseUser(ctx, "er-1", "victim", "home"); err == nil {
		t.Fatal("EraseUser succeeded despite injected failure")
	}
	if n, err := st.CountUserRows(ctx, "victim"); err != nil || n != 3 {
		t.Fatalf("rows after rollback = %d, %v; want 3 (blob, policy, updated_by)", n, err)
	}
	var by string
	if err := st.db.QueryRow(`SELECT updated_by FROM parental_policies WHERE user_id='kid'`).Scan(&by); err != nil || by != "victim" {
		t.Fatalf("anonymisation not rolled back: %q %v", by, err)
	}
	if ok, err := st.ErasureApplied(ctx, "er-1"); err != nil || ok {
		t.Fatalf("applied record exists after rollback: %v %v", ok, err)
	}
	if erased, err := st.UserErased(ctx, "victim"); err != nil || erased {
		t.Fatalf("user refused after failed erasure: %v %v", erased, err)
	}
	if _, err := st.db.Exec(`DROP TRIGGER inject_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EraseUser(ctx, "er-1", "victim", "home"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n, err := st.CountUserRows(ctx, "victim"); err != nil || n != 0 {
		t.Fatalf("rows after retry = %d, %v", n, err)
	}
}

func TestErasedUserWritesRefusedAndSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "userdata.db")
	st := policyStore(t, path)
	if _, _, err := st.UpsertProgress("victim", models.ProgressEntry{ID: "m1", Kind: models.MediaKindMovie, Title: "T", Href: "/m1", UpdatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EraseUser(ctx, "er-1", "victim", "home"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// A new process: nothing in memory, only the table.
	st = policyStore(t, path)
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrUserErased) {
			t.Errorf("%s: err = %v, want ErrUserErased", what, err)
		}
	}
	_, _, err := st.Put("victim", models.Blob{Favorites: map[string]models.FavoriteEntry{"x": {ID: "x"}}})
	refused("Put", err)
	_, _, err = st.UpsertProgress("victim", models.ProgressEntry{ID: "m2", Kind: models.MediaKindMovie, Title: "T", Href: "/m2", UpdatedAt: "2026-01-01T00:00:00Z"})
	refused("UpsertProgress", err)
	_, _, _, err = st.ToggleFavorite("victim", models.FavoriteEntry{ID: "f", Kind: models.MediaKindMovie, Title: "F", Href: "/f"})
	refused("ToggleFavorite", err)
	_, err = st.PutParentalPolicy(ctx, parental.Scope{TenantID: "home", UserID: "victim"}, "parent", unrestrictedUpdate(0))
	refused("PutParentalPolicy target", err)
	_, err = st.PutParentalPolicy(ctx, parental.Scope{TenantID: "home", UserID: "kid"}, "victim", unrestrictedUpdate(0))
	refused("PutParentalPolicy actor", err)
	if n, err := st.CountUserRows(ctx, "victim"); err != nil || n != 0 {
		t.Fatalf("refused writes left %d rows, %v", n, err)
	}
	if d, err := st.GetParentalPolicy(ctx, parental.Scope{TenantID: "home", UserID: "kid"}); err != nil || d.State != "unconfigured" {
		t.Fatalf("refused actor write created a policy: %+v %v", d, err)
	}
	// Other users still work.
	if _, _, err := st.Put("bystander", models.Blob{Favorites: map[string]models.FavoriteEntry{"x": {ID: "x"}}}); err != nil {
		t.Fatalf("bystander write: %v", err)
	}
	if _, err := st.PutParentalPolicy(ctx, parental.Scope{TenantID: "home", UserID: "kid"}, "parent", unrestrictedUpdate(0)); err != nil {
		t.Fatalf("bystander policy write: %v", err)
	}
}

func TestErasureAppliedMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "userdata.db")
	for range 3 {
		st, err := New(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name FROM pragma_table_info('erasure_applied') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	want := []string{"erasure_id", "user_id", "tenant_id", "applied_at", "counts_json"}
	if len(got) != len(want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("columns = %v, want %v", got, want)
		}
	}
}
