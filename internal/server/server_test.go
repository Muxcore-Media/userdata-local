package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Muxcore-Media/core/pkg/contracts"
	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"github.com/Muxcore-Media/userdata-local/internal/models"
	"github.com/Muxcore-Media/userdata-local/internal/server"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	userdatav1 "github.com/Muxcore-Media/userdata-local/proto/gen/muxcore/userdata/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func testServer(t *testing.T) *server.Server {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "userdata.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	provider := auth.NewStaticProvider(map[string]contracts.Session{
		"alice-token": {UserID: "alice", Roles: []string{"user"}},
		"bob-token":   {UserID: "bob", Roles: []string{"user"}},
	})
	return server.New(st, auth.NewGuard(provider))
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
	putReq.Header.Set("Authorization", "Bearer alice-token")
	putReq.Header.Set("X-MuxCore-User-Id", "alice")
	putRec := httptest.NewRecorder()
	mux.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/userdata", nil)
	getReq.Header.Set("Authorization", "Bearer alice-token")
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

func TestHTTPUserdataRejectsSpoofedUserID(t *testing.T) {
	srv := testServer(t)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/userdata", nil)
	req.Header.Set("Authorization", "Bearer alice-token")
	req.Header.Set("X-MuxCore-User-Id", "bob")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHTTPUserdataRejectsMissingBearer(t *testing.T) {
	srv := testServer(t)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/userdata", nil)
	req.Header.Set("X-MuxCore-User-Id", "alice")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGRPCPutListContinueWatching(t *testing.T) {
	srv := testServer(t)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.AuthTokenMetadataKey, "bob-token"))
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

func TestGRPCRejectsSpoofedUserID(t *testing.T) {
	srv := testServer(t)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.AuthTokenMetadataKey, "alice-token"))

	_, err := srv.Get(ctx, &userdatav1.GetRequest{UserId: "bob"})
	if err == nil {
		t.Fatal("expected permission denied")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code=%v err=%v", status.Code(err), err)
	}
}

func TestGRPCAllowsMeshCaller(t *testing.T) {
	srv := testServer(t)
	ctx := auth.VerifiedMeshContext("muxcore")

	_, err := srv.Get(ctx, &userdatav1.GetRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
}
