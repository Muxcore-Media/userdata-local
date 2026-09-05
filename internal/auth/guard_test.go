package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Muxcore-Media/core/pkg/contracts"
	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func testGuard() *auth.Guard {
	provider := auth.NewStaticProvider(map[string]contracts.Session{
		"alice-token": {UserID: "alice", Roles: []string{"user"}},
		"admin-token": {UserID: "admin", Roles: []string{"admin"}},
	})
	return auth.NewGuard(provider)
}

func TestAuthenticateHTTP_AllowsMatchingUser(t *testing.T) {
	guard := testGuard()
	req := httptest.NewRequest(http.MethodGet, "/api/userdata", nil)
	req.Header.Set("Authorization", "Bearer alice-token")
	req.Header.Set(auth.UserIDHeader, "alice")

	session, userID, err := guard.AuthenticateHTTP(req)
	if err != nil {
		t.Fatalf("AuthenticateHTTP: %v", err)
	}
	if session.UserID != "alice" || userID != "alice" {
		t.Fatalf("session=%+v userID=%q", session, userID)
	}
}

func TestAuthenticateHTTP_RejectsSpoofedUserID(t *testing.T) {
	guard := testGuard()
	req := httptest.NewRequest(http.MethodGet, "/api/userdata", nil)
	req.Header.Set("Authorization", "Bearer alice-token")
	req.Header.Set(auth.UserIDHeader, "bob")

	_, _, err := guard.AuthenticateHTTP(req)
	if err == nil {
		t.Fatal("expected error for spoofed user id")
	}
}

func TestAuthenticateHTTP_RejectsMissingBearer(t *testing.T) {
	guard := testGuard()
	req := httptest.NewRequest(http.MethodGet, "/api/userdata", nil)
	req.Header.Set(auth.UserIDHeader, "alice")

	_, _, err := guard.AuthenticateHTTP(req)
	if err == nil {
		t.Fatal("expected error for missing bearer token")
	}
}

func TestAuthenticateHTTP_AllowsAdminForOtherUser(t *testing.T) {
	guard := testGuard()
	req := httptest.NewRequest(http.MethodGet, "/api/userdata", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set(auth.UserIDHeader, "bob")

	_, userID, err := guard.AuthenticateHTTP(req)
	if err != nil {
		t.Fatalf("AuthenticateHTTP: %v", err)
	}
	if userID != "bob" {
		t.Fatalf("userID=%q", userID)
	}
}

func TestAuthorizeGRPC_RejectsSpoofedUserWithoutAuth(t *testing.T) {
	guard := testGuard()
	ctx := context.Background()

	err := guard.AuthorizeGRPC(ctx, "alice")
	if err == nil {
		t.Fatal("expected unauthenticated error")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code=%v err=%v", status.Code(err), err)
	}
}

func TestAuthorizeGRPC_RejectsMismatchedSessionUser(t *testing.T) {
	guard := testGuard()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.AuthTokenMetadataKey, "alice-token"))

	err := guard.AuthorizeGRPC(ctx, "bob")
	if err == nil {
		t.Fatal("expected permission denied")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code=%v err=%v", status.Code(err), err)
	}
}

func TestAuthorizeGRPC_AllowsMatchingSessionUser(t *testing.T) {
	guard := testGuard()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.AuthTokenMetadataKey, "alice-token"))

	if err := guard.AuthorizeGRPC(ctx, "alice"); err != nil {
		t.Fatalf("AuthorizeGRPC: %v", err)
	}
}

func TestAuthorizeGRPC_AllowsVerifiedMeshCaller(t *testing.T) {
	guard := testGuard()
	ctx := auth.VerifiedMeshContext("muxcore")

	if err := guard.AuthorizeGRPC(ctx, "any-user"); err != nil {
		t.Fatalf("AuthorizeGRPC: %v", err)
	}
}

func TestAuthorizeGRPC_RejectsSpoofedMeshMetadata(t *testing.T) {
	guard := testGuard()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.CallerIDMetadataKey, "muxcore"))

	err := guard.AuthorizeGRPC(ctx, "alice")
	if err == nil {
		t.Fatal("expected unauthenticated error for metadata-only caller")
	}
}
