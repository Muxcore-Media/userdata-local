// Package httptransport implements the userdata HTTP transport boundary. It is
// deliberately independent of the legacy gRPC TLS configuration.
package httptransport

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const Provider = "userdata-local"

type Config struct {
	Profile  string
	Insecure bool
	ModuleID string
	CertFile string
	KeyFile  string
	CAFile   string
}

// ValidateMode is called by both the HTTP listener and client constructors.
// Dev without an explicit insecure flag still uses TLS.
func ValidateMode(profile string, insecure bool) error {
	profile = strings.ToLower(strings.TrimSpace(profile))
	switch profile {
	case "":
		if insecure {
			slog.Warn("userdata HTTP: unset profile with insecure flag infers dev; set MUXCORE_PROFILE explicitly")
		}
	case "dev":
	case "household", "staging":
		if insecure {
			return fmt.Errorf("userdata HTTP: insecure transport is forbidden in household/staging")
		}
	default:
		return fmt.Errorf("userdata HTTP: unknown security profile")
	}
	if insecure {
		slog.Warn("userdata HTTP: insecure dev transport has no authenticated module identity")
	}
	return nil
}

// FromEnv resolves only existing identity paths. It never enrolls, changes the
// environment, reads bootstrap tokens, generates material or uses system roots.
func FromEnv(moduleID string) (Config, error) {
	cfg := Config{Profile: os.Getenv("MUXCORE_PROFILE"), ModuleID: moduleID}
	for _, name := range []string{"MUXCORE_INSECURE_DISABLE_TLS", "MUXCORE_GRPC_INSECURE", "MUXCORE_DEV_TLS_SKIP"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
		case "", "false", "0":
		case "true", "1":
			cfg.Insecure = true
		default:
			return Config{}, fmt.Errorf("userdata HTTP: invalid boolean %s", name)
		}
	}
	if err := ValidateMode(cfg.Profile, cfg.Insecure); err != nil {
		return Config{}, err
	}
	if cfg.Insecure {
		return cfg, nil
	}
	cfg.CertFile = strings.TrimSpace(os.Getenv("MUXCORE_TLS_CERT"))
	cfg.KeyFile = strings.TrimSpace(os.Getenv("MUXCORE_TLS_KEY"))
	cfg.CAFile = strings.TrimSpace(os.Getenv("MUXCORE_TLS_CA"))
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return Config{}, fmt.Errorf("userdata HTTP: both MUXCORE_TLS_CERT and MUXCORE_TLS_KEY are required")
	}
	dir := strings.TrimSpace(os.Getenv("MUXCORE_TLS_DIR"))
	if cfg.CertFile == "" && dir != "" {
		cfg.CertFile, cfg.KeyFile = filepath.Join(dir, "module.crt"), filepath.Join(dir, "module.key")
	}
	if cfg.CAFile == "" {
		if caDir := strings.TrimSpace(os.Getenv("MUXCORE_CA_EXPORT_DIR")); caDir != "" {
			cfg.CAFile = filepath.Join(caDir, "ca.crt")
		} else if dir != "" {
			cfg.CAFile = filepath.Join(dir, "ca.crt")
		}
	}
	return cfg, nil
}

func LoadIdentity(cfg Config, usage x509.ExtKeyUsage) (tls.Certificate, *x509.CertPool, error) {
	if cfg.ModuleID == "" || cfg.CertFile == "" || cfg.KeyFile == "" || cfg.CAFile == "" {
		return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: module identity, certificate, key and explicit CA are required")
	}
	pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: load identity: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: read CA: %w", err)
	}
	roots := x509.NewCertPool()
	count := 0
	for len(bytes.TrimSpace(caPEM)) > 0 {
		if !bytes.HasPrefix(bytes.TrimSpace(caPEM), []byte("-----BEGIN CERTIFICATE-----")) {
			return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: malformed CA PEM")
		}
		block, rest := pem.Decode(caPEM)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: malformed CA PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA {
			return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: invalid CA certificate")
		}
		roots.AddCert(cert)
		count++
		caPEM = rest
	}
	if count == 0 {
		return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: empty CA pool")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: parse identity: %w", err)
	}
	if leaf.Subject.CommonName != cfg.ModuleID {
		return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: certificate does not identify configured module")
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: malformed certificate chain")
		}
		intermediates.AddCert(cert)
	}
	opts := x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{usage}}
	if usage == x509.ExtKeyUsageServerAuth {
		opts.DNSName = Provider
	}
	if _, err := leaf.Verify(opts); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: invalid identity: %w", err)
	}
	pair.Leaf = leaf
	return pair, roots, nil
}

func ServerConfig(cfg Config) (*tls.Config, error) {
	if err := ValidateMode(cfg.Profile, cfg.Insecure); err != nil {
		return nil, err
	}
	if cfg.Insecure {
		return nil, nil
	}
	if cfg.ModuleID != Provider {
		return nil, fmt.Errorf("userdata HTTP: provider identity must be userdata-local")
	}
	pair, roots, err := LoadIdentity(cfg, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
	}, nil
}
