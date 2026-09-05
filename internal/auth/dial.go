package auth

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	envAuthLocalGRPCAddr  = "AUTH_LOCAL_GRPC_ADDR"
	envInsecureDisableTLS = "MUXCORE_INSECURE_DISABLE_TLS"
	envGRPCInsecure       = "MUXCORE_GRPC_INSECURE"
	envTLSCert            = "MUXCORE_TLS_CERT"
	envTLSKey             = "MUXCORE_TLS_KEY"
	envTLSCA              = "MUXCORE_TLS_CA"
)

// DefaultAuthLocalAddr is auth-local's default gRPC listen address.
const DefaultAuthLocalAddr = "localhost:9403"

// ResolveAuthLocalAddr returns the auth-local gRPC dial target.
func ResolveAuthLocalAddr() string {
	if v := strings.TrimSpace(os.Getenv(envAuthLocalGRPCAddr)); v != "" {
		return v
	}
	return DefaultAuthLocalAddr
}

func insecureAllowed() bool {
	for _, key := range []string{envInsecureDisableTLS, envGRPCInsecure} {
		v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
		if v == "true" || v == "1" {
			return true
		}
	}
	return false
}

func isLocalhostAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == ""
}

// DialAuthLocal opens a gRPC connection to auth-local for session validation.
func DialAuthLocal(addr string) (*grpc.ClientConn, error) {
	if addr == "" {
		addr = ResolveAuthLocalAddr()
	}
	var opts []grpc.DialOption
	if insecureAllowed() && isLocalhostAddr(addr) {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		creds, err := loadClientTLS()
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}
	return grpc.NewClient(addr, opts...)
}

func loadClientTLS() (credentials.TransportCredentials, error) {
	certFile := strings.TrimSpace(os.Getenv(envTLSCert))
	keyFile := strings.TrimSpace(os.Getenv(envTLSKey))
	caFile := strings.TrimSpace(os.Getenv(envTLSCA))
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("TLS required for auth-local dial — set %s/%s or %s=true for localhost dev",
			envTLSCert, envTLSKey, envInsecureDisableTLS)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client TLS cert/key: %w", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("parse TLS CA from %q", caFile)
		}
		tlsConfig.RootCAs = pool
	}
	return credentials.NewTLS(tlsConfig), nil
}
