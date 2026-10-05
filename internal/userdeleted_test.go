package internal

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Muxcore-Media/contracts-media/events"
	"github.com/Muxcore-Media/userdata-local/internal/models"
	"github.com/Muxcore-Media/userdata-local/internal/store"
)

func TestDeleteAccountRemovesProfileSiblings(t *testing.T) {
	st := openStore(t)
	for _, id := range []string{"acct", "acct" + store.ProfileBlobSep + "abcdabcdabcdabcd", "acct2", "other", "a_b", "axb"} {
		if _, _, err := st.Put(id, models.Blob{Progress: map[string]models.ProgressEntry{"m": {ID: "m"}}}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}

	n, err := st.DeleteAccount("acct")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted %d, want 2", n)
	}
	for _, id := range []string{"acct", "acct" + store.ProfileBlobSep + "abcdabcdabcdabcd"} {
		blob, rev, err := st.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if rev != 0 || len(blob.Progress) != 0 {
			t.Fatalf("%s still stored rev=%d", id, rev)
		}
	}
	for _, id := range []string{"acct2", "other", "a_b", "axb"} {
		_, rev, err := st.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if rev == 0 {
			t.Fatalf("%s was deleted", id)
		}
	}

	if _, err := st.DeleteAccount(""); err == nil {
		t.Fatal("expected empty id to fail")
	}
	if _, err := st.DeleteAccount("acct" + store.ProfileBlobSep + "abcdabcdabcdabcd"); err == nil {
		t.Fatal("expected profile blob id to fail")
	}
	n, err = st.DeleteAccount("a_b")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("underscore delete %d, want 1", n)
	}
	if _, rev, err := st.Get("axb"); err != nil || rev == 0 {
		t.Fatalf("axb rev=%d err=%v", rev, err)
	}
}

func TestApplyUserDeleted(t *testing.T) {
	st := openStore(t)
	account := "house-1"
	sibling := account + store.ProfileBlobSep + "0123456789abcdef"
	if _, _, err := st.Put(account, models.EmptyBlob()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Put(sibling, models.EmptyBlob()); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(events.UserDeletedPayload{UserID: account})
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyUserDeleted(st, raw); err != nil {
		t.Fatal(err)
	}
	if _, rev, err := st.Get(sibling); err != nil || rev != 0 {
		t.Fatalf("sibling rev=%d err=%v", rev, err)
	}
	if err := ApplyUserDeleted(st, []byte(`{}`)); err == nil {
		t.Fatal("expected missing user_id to fail")
	}
	if err := ApplyUserDeleted(st, []byte(`not-json`)); err == nil {
		t.Fatal("expected bad json to fail")
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "userdata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
