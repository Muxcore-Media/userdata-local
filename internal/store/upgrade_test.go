package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Muxcore-Media/core/sdk/go/module/moduletest"
	"github.com/Muxcore-Media/userdata-local/parental"
)

// TestUpgradeFromSnapshots opens databases written by earlier tags with the
// current code (ADR-0015, NFR-DATA-002, FR-INS-005).
func TestUpgradeFromSnapshots(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v0.1.6"} {
		t.Run(tag, func(t *testing.T) {
			path := moduletest.CopyFixture(t, filepath.Join("testdata", "upgrade", tag+".db"))

			// Open twice: startup migration must be idempotent.
			first, err := New(path)
			if err != nil {
				t.Fatalf("first open: %v", err)
			}
			if err := first.Close(); err != nil {
				t.Fatalf("first close: %v", err)
			}
			st, err := New(path)
			if err != nil {
				t.Fatalf("second open: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })

			fresh, err := New(filepath.Join(t.TempDir(), "fresh.db"))
			if err != nil {
				t.Fatalf("fresh open: %v", err)
			}
			t.Cleanup(func() { _ = fresh.Close() })
			moduletest.RequireSchemaSuperset(t, moduletest.Schema(t, st.db), moduletest.Schema(t, fresh.db))

			// Seeded rows read back through the current API.
			blob, rev, err := st.Get("alice@example.com")
			if err != nil {
				t.Fatalf("Get alice: %v", err)
			}
			if rev != 2 {
				t.Errorf("alice revision = %d, want 2", rev)
			}
			if len(blob.Progress) != 2 || blob.Progress["movie-1"].Title != "Heat" ||
				blob.Progress["movie-1"].PositionSec != 120 || blob.Progress["ep-9"].Watched == nil || !*blob.Progress["ep-9"].Watched {
				t.Errorf("alice progress mismatch: %+v", blob.Progress)
			}
			if fav := blob.Favorites["movie-2"]; fav.Title != "Matrix" || fav.Year == nil || *fav.Year != 1999 {
				t.Errorf("alice favorite mismatch: %+v", fav)
			}
			if len(blob.Playlists) != 1 || blob.Playlists[0].Name != "Weekend" || len(blob.Playlists[0].ItemIDs) != 2 {
				t.Errorf("alice playlists mismatch: %+v", blob.Playlists)
			}
			if len(blob.Queue) != 1 || blob.Queue[0].ID != "movie-2" {
				t.Errorf("alice queue mismatch: %+v", blob.Queue)
			}
			if blob.Prefs == nil || blob.Prefs.Display.Theme != "light" || blob.Prefs.Subtitles.Language != "fra" {
				t.Errorf("alice prefs mismatch: %+v", blob.Prefs)
			}
			if blob.Prefs != nil && string(blob.Prefs.Parental) != `{"kids_mode":true,"max_parental_rating":"PG","pin_hash":"abc123"}` {
				t.Errorf("alice parental prefs lost: %s", blob.Prefs.Parental)
			}
			bob, rev, err := st.Get("bob")
			if err != nil || rev != 1 || len(bob.Progress) != 2 {
				t.Errorf("bob: rev=%d progress=%d err=%v", rev, len(bob.Progress), err)
			}
			cw, err := st.ListContinueWatching("alice@example.com", 10)
			if err != nil || len(cw) == 0 {
				t.Errorf("continue watching: %v %v", cw, err)
			}

			// Defaults: a row inserted without optional columns gets revision 1 and a timestamp.
			if _, err := st.db.Exec(`INSERT INTO user_blobs (user_id, json_blob) VALUES ('carol', x'7b7d')`); err != nil {
				t.Fatalf("insert defaults row: %v", err)
			}
			var rv int64
			var ts string
			if err := st.db.QueryRow(`SELECT revision, updated_at FROM user_blobs WHERE user_id='carol'`).Scan(&rv, &ts); err != nil {
				t.Fatal(err)
			}
			if rv != 1 || ts == "" {
				t.Errorf("column defaults: revision=%d updated_at=%q", rv, ts)
			}

			// Writes still work on the upgraded DB.
			if _, rev, err := st.MarkWatched("bob", "movie-1", true); err != nil || rev != 2 {
				t.Errorf("MarkWatched after upgrade: rev=%d err=%v", rev, err)
			}

			// v0.1.5+ snapshots carry parental_policies rows (ADR-0030).
			hasPolicies := tag != "v0.1.0"
			if hasPolicies {
				doc, err := st.GetParentalPolicy(context.Background(), parental.Scope{TenantID: "home", UserID: "kid"})
				if err != nil || doc.State != "configured" || doc.Revision != 1 {
					t.Errorf("kid policy after upgrade: %+v %v", doc, err)
				}
			}

			// ADR-0035: the erasure_applied table is created by the forward
			// migration and the erasure works on the upgraded database.
			if ok, err := st.ErasureApplied(context.Background(), "er-upgrade"); err != nil || ok {
				t.Fatalf("erasure_applied after upgrade: %v %v", ok, err)
			}
			counts, err := st.EraseUser(context.Background(), "er-upgrade", "alice@example.com", "home")
			if err != nil {
				t.Fatalf("EraseUser after upgrade: %v", err)
			}
			wantPolicies, wantAnon := int64(0), int64(0)
			if hasPolicies {
				wantPolicies, wantAnon = 1, 1
			}
			if counts[CountUserBlobs] != 1 || counts[CountParentalPolicies] != wantPolicies || counts[CountParentalPoliciesAnonymise] != wantAnon {
				t.Errorf("erase counts after upgrade: %v", counts)
			}
			if n, err := st.CountUserRows(context.Background(), "alice@example.com"); err != nil || n != 0 {
				t.Errorf("post-condition after upgrade = %d, %v", n, err)
			}
			if _, rev, err := st.Get("bob"); err != nil || rev != 2 {
				t.Errorf("bystander bob after erasure: rev=%d err=%v", rev, err)
			}
			if hasPolicies {
				var by string
				if err := st.db.QueryRow(`SELECT updated_by FROM parental_policies WHERE user_id='kid'`).Scan(&by); err != nil || by != AnonymisedActor {
					t.Errorf("kid updated_by = %q, %v", by, err)
				}
				if doc, err := st.GetParentalPolicy(context.Background(), parental.Scope{TenantID: "home", UserID: "bob"}); err != nil || doc.State != "configured" {
					t.Errorf("bob policy after erasure: %+v %v", doc, err)
				}
			}
			moduletest.RequireIntegrity(t, st.db)
		})
	}
}
