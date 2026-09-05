package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Muxcore-Media/core/pkg/contracts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const UserIDHeader = "X-MuxCore-User-Id"

// Guard validates caller identity for HTTP and gRPC userdata access.
type Guard struct {
	provider contracts.AuthProvider
}

// NewGuard constructs a guard backed by an auth-local AuthProvider.
func NewGuard(provider contracts.AuthProvider) *Guard {
	return &Guard{provider: provider}
}

// AuthenticateHTTP validates the bearer session and ensures the header user id
// matches the authenticated principal (or the caller has the admin role).
func (g *Guard) AuthenticateHTTP(r *http.Request) (contracts.Session, string, error) {
	if g == nil || g.provider == nil {
		return contracts.Session{}, "", fmt.Errorf("auth not configured")
	}
	token := bearerTokenFromRequest(r)
	if token == "" {
		return contracts.Session{}, "", fmt.Errorf("missing bearer token")
	}
	session, err := g.provider.Validate(r.Context(), token)
	if err != nil {
		return contracts.Session{}, "", fmt.Errorf("invalid session: %w", err)
	}
	if strings.TrimSpace(session.UserID) == "" {
		return contracts.Session{}, "", fmt.Errorf("session has no user id")
	}
	headerUserID := strings.TrimSpace(r.Header.Get(UserIDHeader))
	if headerUserID == "" {
		return contracts.Session{}, "", fmt.Errorf("missing %s header", UserIDHeader)
	}
	if err := authorizeUserAccess(session, headerUserID); err != nil {
		return contracts.Session{}, "", err
	}
	return session, headerUserID, nil
}

// AuthorizeGRPC ensures the caller may access targetUserID.
// Verified mesh peers (mTLS client cert CN) are trusted service accounts.
// Otherwise a bearer/x-auth-token session must match targetUserID or be admin.
func (g *Guard) AuthorizeGRPC(ctx context.Context, targetUserID string) error {
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}
	if _, ok := meshCallerFromContext(ctx); ok {
		return nil
	}
	if g == nil || g.provider == nil {
		return status.Error(codes.Unauthenticated, "auth not configured")
	}
	token := sessionTokenFromContext(ctx)
	if token == "" {
		return status.Error(codes.Unauthenticated, "missing auth token")
	}
	session, err := g.provider.Validate(ctx, token)
	if err != nil {
		return status.Error(codes.Unauthenticated, "invalid or expired session")
	}
	if err := authorizeUserAccessStatus(session, targetUserID); err != nil {
		return err
	}
	return nil
}

func authorizeUserAccess(session contracts.Session, targetUserID string) error {
	if session.UserID == targetUserID {
		return nil
	}
	if hasRole(session.Roles, "admin") {
		return nil
	}
	return fmt.Errorf("user id does not match authenticated principal")
}

func authorizeUserAccessStatus(session contracts.Session, targetUserID string) error {
	if session.UserID == targetUserID {
		return nil
	}
	if hasRole(session.Roles, "admin") {
		return nil
	}
	return status.Error(codes.PermissionDenied, "permission denied")
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}
