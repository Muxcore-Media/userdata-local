package auth

import (
	"context"
	"fmt"

	"github.com/Muxcore-Media/core/pkg/contracts"
	authv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/auth/v1"
	"google.golang.org/grpc"
)

// SidecarAuthProvider wraps a gRPC connection to auth-local and implements
// contracts.AuthProvider by delegating Validate/Authenticate to AuthService.
type SidecarAuthProvider struct {
	client authv1.AuthServiceClient
}

// NewSidecarAuthProvider constructs a provider over an existing auth-local connection.
func NewSidecarAuthProvider(conn *grpc.ClientConn) *SidecarAuthProvider {
	return &SidecarAuthProvider{client: authv1.NewAuthServiceClient(conn)}
}

func (s *SidecarAuthProvider) Authenticate(ctx context.Context, creds contracts.Credentials) (contracts.Session, error) {
	resp, err := s.client.Authenticate(ctx, &authv1.AuthenticateRequest{
		CredentialType: creds.Type,
		CredentialData: creds.Data,
	})
	if err != nil {
		return contracts.Session{}, fmt.Errorf("sidecar auth: %w", err)
	}
	if !resp.Authenticated {
		return contracts.Session{}, fmt.Errorf("authentication failed: %s", resp.Error)
	}
	return contracts.Session{
		UserID:      resp.UserId,
		Username:    resp.Username,
		Roles:       resp.Roles,
		Permissions: resp.Permissions,
		Token:       resp.SessionToken,
	}, nil
}

func (s *SidecarAuthProvider) Validate(ctx context.Context, token string) (contracts.Session, error) {
	resp, err := s.client.Validate(ctx, &authv1.ValidateRequest{Token: token})
	if err != nil {
		return contracts.Session{}, fmt.Errorf("sidecar auth validate: %w", err)
	}
	if !resp.Valid {
		return contracts.Session{}, fmt.Errorf("invalid token: %s", resp.Error)
	}
	return contracts.Session{
		UserID:      resp.UserId,
		Username:    resp.Username,
		Roles:       resp.Roles,
		Permissions: resp.Permissions,
	}, nil
}

func (s *SidecarAuthProvider) Revoke(ctx context.Context, token string) error {
	_, err := s.client.Revoke(ctx, &authv1.RevokeRequest{Token: token})
	return err
}
