package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Muxcore-Media/core/pkg/contracts"
	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"github.com/Muxcore-Media/userdata-local/internal/server"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	userdatav1 "github.com/Muxcore-Media/userdata-local/proto/gen/muxcore/userdata/v1"
)

// An erased user's late PUT is refused with a stable code on every write path
// (ADR-0035) and the error does not depend on the auth provider still knowing
// the account.
func TestLatePutAfterErasureIsRefused(t *testing.T) {
	mux, _, st := policyHTTPFixture(t)
	ctx := context.Background()
	// "kid" has data, then the erasure is applied.
	put := func(method, path, token, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(auth.UserIDHeader, target)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	if w := put(http.MethodPut, "/api/userdata", "kid", "kid", `{"favorites":{"x":{"id":"x"}}}`); w.Code != http.StatusOK {
		t.Fatalf("seed blob: %d %s", w.Code, w.Body.String())
	}
	if w := put(http.MethodPut, "/api/parental-policy", "admin", "kid", unrestrictedBody); w.Code != http.StatusOK {
		t.Fatalf("seed policy: %d %s", w.Code, w.Body.String())
	}
	if _, err := st.EraseUser(ctx, "er-1", "kid", "a"); err != nil {
		t.Fatal(err)
	}

	w := put(http.MethodPut, "/api/userdata", "kid", "kid", `{"favorites":{"y":{"id":"y"}}}`)
	var body map[string]string
	if w.Code != http.StatusGone || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["code"] != server.CodeUserdataAccountErased {
		t.Fatalf("late userdata PUT: %d %s", w.Code, w.Body.String())
	}
	w = put(http.MethodPut, "/api/parental-policy", "admin", "kid", unrestrictedBody)
	body = nil
	if w.Code != http.StatusGone || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["code"] != server.CodePolicyAccountErased {
		t.Fatalf("late policy PUT: %d %s", w.Code, w.Body.String())
	}
	// The erased admin as actor is refused too (updated_by cannot be re-introduced).
	if _, err := st.EraseUser(ctx, "er-2", "parent", "a"); err != nil {
		t.Fatal(err)
	}
	w = put(http.MethodPut, "/api/parental-policy", "admin", "kid", unrestrictedBody)
	if w.Code != http.StatusGone {
		t.Fatalf("erased actor PUT: %d %s", w.Code, w.Body.String())
	}
	if n, err := st.CountUserRows(ctx, "kid"); err != nil || n != 0 {
		t.Fatalf("rows for erased user = %d, %v", n, err)
	}
	// Reads still answer (empty), nothing is resurrected.
	if w := put(http.MethodGet, "/api/userdata", "kid", "kid", ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"x"`) {
		t.Fatalf("read after erasure: %d %s", w.Code, w.Body.String())
	}
}

func TestGRPCLatePutAfterErasureIsRefused(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "userdata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(st, auth.NewGuard(auth.NewStaticProvider(map[string]contracts.Session{
		"alice-token": {UserID: "alice", Roles: []string{"user"}},
		"bob-token":   {UserID: "bob", Roles: []string{"user"}},
	})))
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.AuthTokenMetadataKey, "alice-token"))
	if _, err := srv.Put(ctx, &userdatav1.PutRequest{UserId: "alice", Json: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EraseUser(ctx, "er-1", "alice", ""); err != nil {
		t.Fatal(err)
	}
	_, err = srv.Put(ctx, &userdatav1.PutRequest{UserId: "alice", Json: []byte(`{"favorites":{"x":{"id":"x"}}}`)})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != server.CodeUserdataAccountErased {
		t.Fatalf("late gRPC Put: %v", err)
	}
	// Bystander unaffected.
	bob := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.AuthTokenMetadataKey, "bob-token"))
	if _, err := srv.Put(bob, &userdatav1.PutRequest{UserId: "bob", Json: []byte(`{}`)}); err != nil {
		t.Fatalf("bystander Put: %v", err)
	}
}
