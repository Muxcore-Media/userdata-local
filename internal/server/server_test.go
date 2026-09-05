package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Muxcore-Media/userdata-local/internal/models"
	"github.com/Muxcore-Media/userdata-local/internal/server"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	userdatav1 "github.com/Muxcore-Media/userdata-local/proto/gen/muxcore/userdata/v1"
)

func testServer(t *testing.T) *server.Server {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "userdata.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return server.New(st)
}

func TestHTTPUserdataPutGet(t *testing.T) {
	srv := testServer(t)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	watched := false
	body, _ := json.Marshal(models.Blob{
		Progress: map[string]models.ProgressEntry{
			"m1": {
				ID: "m1", Kind: models.MediaKindMovie, Title: "Movie", Href: "/movies/m1",
				PositionSec: 42, DurationSec: 100, UpdatedAt: "2026-01-01T00:00:00Z", Watched: &watched,
			},
		},
	})
	putReq := httptest.NewRequest(http.MethodPut, "/api/userdata", bytes.NewReader(body))
	putReq.Header.Set("X-MuxCore-User-Id", "alice")
	putRec := httptest.NewRecorder()
	mux.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/userdata", nil)
	getReq.Header.Set("X-MuxCore-User-Id", "alice")
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status=%d", getRec.Code)
	}
	var blob models.Blob
	if err := json.Unmarshal(getRec.Body.Bytes(), &blob); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if blob.Progress["m1"].PositionSec != 42 {
		t.Fatalf("progress=%+v", blob.Progress["m1"])
	}
}

func TestGRPCPutListContinueWatching(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	watched := false
	in := models.Blob{
		Progress: map[string]models.ProgressEntry{
			"m1": {
				ID: "m1", Kind: models.MediaKindMovie, Title: "Movie", Href: "/movies/m1",
				PositionSec: 100, DurationSec: 1000, UpdatedAt: "2026-01-04T00:00:00Z", Watched: &watched,
			},
		},
	}
	raw, _ := json.Marshal(in)
	if _, err := srv.Put(ctx, &userdatav1.PutRequest{UserId: "bob", Json: raw}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	resp, err := srv.ListContinueWatching(ctx, &userdatav1.ListContinueWatchingRequest{UserId: "bob", Limit: 5})
	if err != nil {
		t.Fatalf("ListContinueWatching: %v", err)
	}
	var items []models.ProgressEntry
	if err := json.Unmarshal(resp.GetJson(), &items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 1 || items[0].ID != "m1" {
		t.Fatalf("items=%+v", items)
	}
}
