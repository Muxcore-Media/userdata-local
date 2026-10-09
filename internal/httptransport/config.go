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

// Environment variables read by FromEnv. Their names and semantics mirror the
// SDK's meshid package and core's profile package (ADR-0016): they are copied
// rather than imported because core's profile package is internal.
const (
	envProfile        = "MUXCORE_PROFILE"
	envInsecure       = "MUXCORE_INSECURE_DISABLE_TLS"
	envInsecureLegacy = "MUXCORE_DEV_TLS_SKIP"
	envModuleID       = "MUXCORE_MODULE_ID"
	envTLSCert        = "MUXCORE_TLS_CERT"
	envTLSKey         = "MUXCORE_TLS_KEY"
	envTLSCA          = "MUXCORE_TLS_CA"
	envTLSDir         = "MUXCORE_TLS_DIR"
	envCAExportDir    = "MUXCORE_CA_EXPORT_DIR"
	envDataDir        = "MUXCORE_DATA_DIR"
)

// InsecureFromEnv reports whether the insecure flag is set exactly as core
// (profile.insecureFlag) and the SDK (meshid.InsecureFromEnv) read it:
// MUXCORE_INSECURE_DISABLE_TLS or the deprecated MUXCORE_DEV_TLS_SKIP, trimmed
// of surrounding space, equal to exactly "true" or "1". Anything else (other
// case, "yes", a typo) is not insecure, and MUXCORE_GRPC_INSECURE is not
// consulted: core would resolve household and the SDK would enroll over mTLS,
// so the HTTP listener must be secure too.
func InsecureFromEnv(getenv func(string) string) bool {
	for _, name := range []string{envInsecure, envInsecureLegacy} {
		if v := strings.TrimSpace(getenv(name)); v == "true" || v == "1" {
			return true
		}
	}
	return false
}

// FromEnv resolves only existing identity paths, following the SDK's
// meshid.Ensure resolution without ever enrolling, changing the environment,
// reading bootstrap tokens, generating material or using system roots:
//
//   - MUXCORE_TLS_CERT and MUXCORE_TLS_KEY (both) are used as given. Unlike
//     meshid, which would silently fall back to the identity directory, exactly
//     one of them is a configuration error.
//   - Otherwise the identity directory is MUXCORE_TLS_DIR, else
//     $MUXCORE_DATA_DIR/mesh-id (MUXCORE_DATA_DIR defaults to ./data, relative
//     to the working directory), holding module.crt and module.key.
//   - The CA is MUXCORE_TLS_CA, else $MUXCORE_CA_EXPORT_DIR/ca.crt when that
//     file exists, else ca.crt in the identity directory.
//
// The certificate is for moduleID, so a different MUXCORE_MODULE_ID override
// (which makes the SDK enroll that CN) is rejected for secure transport.
func FromEnv(moduleID string) (Config, error) {
	return fromEnv(moduleID, os.Getenv)
}

func fromEnv(moduleID string, getenv func(string) string) (Config, error) {
	cfg := Config{Profile: getenv(envProfile), ModuleID: moduleID, Insecure: InsecureFromEnv(getenv)}
	if err := ValidateMode(cfg.Profile, cfg.Insecure); err != nil {
		return Config{}, err
	}
	if cfg.Insecure {
		return cfg, nil
	}
	if override := strings.TrimSpace(getenv(envModuleID)); override != "" && override != moduleID {
		return Config{}, fmt.Errorf("userdata HTTP: %s=%q is unsupported: the HTTP identity is bound to module ID %q "+
			"(the SDK would enroll a certificate with CN %q); unset %s", envModuleID, override, moduleID, override, envModuleID)
	}
	trim := func(name string) string { return strings.TrimSpace(getenv(name)) }
	cfg.CertFile, cfg.KeyFile, cfg.CAFile = trim(envTLSCert), trim(envTLSKey), trim(envTLSCA)
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return Config{}, fmt.Errorf("userdata HTTP: both %s and %s are required", envTLSCert, envTLSKey)
	}
	if cfg.CAFile == "" {
		if caDir := trim(envCAExportDir); caDir != "" {
			if candidate := filepath.Join(caDir, "ca.crt"); fileExists(candidate) {
				cfg.CAFile = candidate
			}
		}
	}
	if cfg.CertFile != "" {
		return cfg, nil
	}
	dir := trim(envTLSDir)
	if dir == "" {
		data := trim(envDataDir)
		if data == "" {
			data = "data"
		}
		dir = filepath.Join(data, "mesh-id")
	}
	cfg.CertFile, cfg.KeyFile = filepath.Join(dir, "module.crt"), filepath.Join(dir, "module.key")
	if cfg.CAFile == "" {
		cfg.CAFile = filepath.Join(dir, "ca.crt")
	}
	return cfg, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
		return tls.Certificate{}, nil, fmt.Errorf("userdata HTTP: certificate CN %q does not identify configured module %q",
			leaf.Subject.CommonName, cfg.ModuleID)
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
		return nil, fmt.Errorf("userdata HTTP: provider identity must be %s, not %q (MUXCORE_MODULE_ID overrides are unsupported)", Provider, cfg.ModuleID)
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
