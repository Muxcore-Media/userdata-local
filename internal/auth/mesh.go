package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"strings"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

const (
	AuthTokenMetadataKey = "x-auth-token"
	CallerIDMetadataKey  = "x-caller-id"
)

func sessionTokenFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(AuthTokenMetadataKey)
	if len(vals) > 0 {
		if token := strings.TrimSpace(vals[0]); token != "" {
			return token
		}
	}
	vals = md.Get("authorization")
	if len(vals) == 0 {
		return ""
	}
	auth := vals[0]
	const prefix = "Bearer "
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return strings.TrimSpace(auth[len(prefix):])
	}
	return strings.TrimSpace(auth)
}

// peerCertCN returns the verified TLS client certificate Common Name from the
// gRPC peer, if present. Mesh identity must come from mTLS — never from
// client-supplied metadata alone.
func peerCertCN(ctx context.Context) (string, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return "", false
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", false
	}
	if len(ti.State.PeerCertificates) == 0 {
		return "", false
	}
	cn := strings.TrimSpace(ti.State.PeerCertificates[0].Subject.CommonName)
	return cn, cn != ""
}

func meshCallerFromContext(ctx context.Context) (string, bool) {
	cn, ok := peerCertCN(ctx)
	if !ok {
		return "", false
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return cn, true
	}
	vals := md.Get(CallerIDMetadataKey)
	if len(vals) == 0 {
		return cn, true
	}
	callerID := strings.TrimSpace(vals[0])
	if callerID == "" {
		return cn, true
	}
	if callerID != cn {
		return "", false
	}
	return cn, true
}

// VerifiedMeshContext builds a test context with TLS peer identity. For tests only.
func VerifiedMeshContext(callerID string) context.Context {
	cert := &x509.Certificate{
		Subject: pkix.Name{CommonName: callerID},
	}
	ti := credentials.TLSInfo{
		State: tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{cert},
		},
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: ti})
	md := metadata.Pairs(CallerIDMetadataKey, callerID)
	return metadata.NewIncomingContext(ctx, md)
}
