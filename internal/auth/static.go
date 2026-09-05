package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/Muxcore-Media/core/pkg/contracts"
)

// StaticProvider validates a fixed set of bearer tokens for offline tests.
type StaticProvider struct {
	sessions map[string]contracts.Session
}

// NewStaticProvider returns an AuthProvider backed by known tokens.
func NewStaticProvider(sessions map[string]contracts.Session) *StaticProvider {
	if sessions == nil {
		sessions = map[string]contracts.Session{}
	}
	return &StaticProvider{sessions: sessions}
}

func (s *StaticProvider) Authenticate(ctx context.Context, credentials contracts.Credentials) (contracts.Session, error) {
	return contracts.Session{}, fmt.Errorf("authenticate not supported in static provider")
}

func (s *StaticProvider) Validate(ctx context.Context, token string) (contracts.Session, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return contracts.Session{}, fmt.Errorf("empty token")
	}
	session, ok := s.sessions[token]
	if !ok {
		return contracts.Session{}, fmt.Errorf("unknown token")
	}
	return session, nil
}

func (s *StaticProvider) Revoke(ctx context.Context, token string) error {
	return fmt.Errorf("revoke not supported in static provider")
}
