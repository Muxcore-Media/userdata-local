package grpctls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	envInsecureDisableTLS = "MUXCORE_INSECURE_DISABLE_TLS"
	envGRPCInsecure       = "MUXCORE_GRPC_INSECURE"
	envTLSCert            = "MUXCORE_TLS_CERT"
	envTLSKey             = "MUXCORE_TLS_KEY"
	envTLSCA              = "MUXCORE_TLS_CA"
	envUserdataTLSCert    = "USERDATA_TLS_CERT"
	envUserdataTLSKey     = "USERDATA_TLS_KEY"
	envUserdataTLSCA      = "USERDATA_TLS_CA"
	envUserdataTLSDir     = "USERDATA_TLS_DIR"
)

// InsecureAllowed reports whether plaintext gRPC is explicitly enabled (dev only).
func InsecureAllowed() bool {
	for _, key := range []string{envInsecureDisableTLS, envGRPCInsecure} {
		v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
		if v == "true" || v == "1" {
			return true
		}
	}
	return false
}

// ServerConfig builds a TLS config for the userdata-local gRPC listener.
// When TLS is required and no cert files are configured, a mesh-local ECDSA CA
// and server certificate are generated under USERDATA_TLS_DIR (or alongside the DB).
func ServerConfig(dataDir string) (*tls.Config, error) {
	if InsecureAllowed() {
		return nil, nil
	}

	certFile := firstNonEmpty(os.Getenv(envUserdataTLSCert), os.Getenv(envTLSCert))
	keyFile := firstNonEmpty(os.Getenv(envUserdataTLSKey), os.Getenv(envTLSKey))
	caFile := firstNonEmpty(os.Getenv(envUserdataTLSCA), os.Getenv(envTLSCA))

	if certFile == "" || keyFile == "" {
		dir := strings.TrimSpace(os.Getenv(envUserdataTLSDir))
		if dir == "" {
			dir = defaultTLSDir(dataDir)
		}
		if err := ensureAutoCerts(dir); err != nil {
			return nil, err
		}
		certFile = filepath.Join(dir, "server.crt")
		keyFile = filepath.Join(dir, "server.key")
		if caFile == "" {
			caFile = filepath.Join(dir, "ca.crt")
		}
		slog.Info("userdata-local gRPC TLS using auto-generated certificates",
			"cert", certFile,
			"ca", caFile,
			"disable", fmt.Sprintf("set %s=true for plaintext dev only", envInsecureDisableTLS),
		)
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS cert/key: %w", err)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.VerifyClientCertIfGiven,
	}

	if caFile != "" {
		pool, err := loadCertPool(caFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
	}

	return cfg, nil
}

func defaultTLSDir(dataDir string) string {
	if dataDir != "" && dataDir != "." {
		return filepath.Join(filepath.Dir(dataDir), "tls", "userdata-local")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "tls/userdata-local"
	}
	return filepath.Join(home, ".muxcore", "tls", "userdata-local")
}

func ensureAutoCerts(dir string) error {
	serverCert := filepath.Join(dir, "server.crt")
	serverKey := filepath.Join(dir, "server.key")
	caCert := filepath.Join(dir, "ca.crt")
	caKey := filepath.Join(dir, "ca.key")

	if fileExists(serverCert) && fileExists(serverKey) && fileExists(caCert) {
		return nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create TLS dir %s: %w", dir, err)
	}

	ca, caPriv, err := generateCA()
	if err != nil {
		return err
	}
	if err := writeCertPair(caCert, caKey, ca, caPriv); err != nil {
		return err
	}

	server, serverPriv, err := signServerCert(ca, caPriv, "userdata-local")
	if err != nil {
		return err
	}
	if err := writeCertPair(serverCert, serverKey, server, serverPriv); err != nil {
		return err
	}
	return nil
}

func generateCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"MuxCore"},
			CommonName:   "MuxCore userdata-local dev CA",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, priv, nil
}

func signServerCert(ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate server key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"MuxCore"},
			CommonName:   cn,
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(825 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:    []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &priv.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("sign server cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, priv, nil
}

func writeCertPair(certPath, keyPath string, cert *x509.Certificate, key *ecdsa.PrivateKey) error {
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write cert %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key %s: %w", keyPath, err)
	}
	return nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read TLS CA %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("parse TLS CA %s", path)
	}
	return pool, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
