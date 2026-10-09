package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muxcore-Media/userdata-local/internal/httptransport"
	"github.com/Muxcore-Media/userdata-local/internal/testtls"
)

func TestPackagedProbeExecutable(t *testing.T) {
	// Build the real standalone command, then invoke it as deployment probes do.
	// It has no daemon/storage/SDK-enrollment dependency.
	binary := filepath.Join(t.TempDir(), "userdata-health")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v %s", err, out)
	}
	ca := testtls.NewCA(t)
	provider := ca.Issue(t, "userdata-local")
	cfg, err := httptransport.ServerConfig(httptransport.Config{Profile: "household", ModuleID: "userdata-local", CertFile: provider.CertFile, KeyFile: provider.KeyFile, CAFile: ca.File})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	s := httptest.NewUnstartedServer(httptransport.Admit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/health" || r.Header.Get("Authorization") != "" {
			t.Error("probe sent data request or credentials")
		}
		if r.TLS.VerifiedChains[0][0].Subject.CommonName != "userdata-local" {
			t.Error("probe borrowed module identity")
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})))
	s.TLS = cfg
	s.StartTLS()
	defer s.Close()
	baseEnv := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "MUXCORE_") && !strings.HasPrefix(key, "USERDATA_") && !strings.HasPrefix(key, "AUTH_LOCAL_") {
			baseEnv = append(baseEnv, entry)
		}
	}
	explicit := map[string]string{"MUXCORE_PROFILE": "household", "MUXCORE_TLS_CERT": provider.CertFile, "MUXCORE_TLS_KEY": provider.KeyFile, "MUXCORE_TLS_CA": ca.File}
	invoke := func(t *testing.T, env map[string]string, wantOK bool, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append([]string(nil), baseEnv...)
		for key, value := range env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantOK {
			t.Fatalf("probe success=%v want %v: %v %s", err == nil, wantOK, err, out)
		}
		if strings.Contains(string(out), "fixture-bootstrap-secret") {
			t.Fatal("probe leaked bootstrap token")
		}
	}
	t.Run("explicit configured paths", func(t *testing.T) { invoke(t, explicit, true, "--origin", s.URL) })
	t.Run("TLS dir and mounted CA", func(t *testing.T) {
		invoke(t, map[string]string{"MUXCORE_PROFILE": "household", "MUXCORE_TLS_DIR": filepath.Dir(provider.CertFile), "MUXCORE_CA_EXPORT_DIR": filepath.Dir(ca.File)}, true, "--origin", s.URL)
	})
	t.Run("local bind to loopback", func(t *testing.T) {
		env := cloneEnv(explicit)
		_, port, err := net.SplitHostPort(strings.TrimPrefix(s.URL, "https://"))
		if err != nil {
			t.Fatal(err)
		}
		env["USERDATA_LOCAL_HTTP_ADDR"] = ":" + port
		invoke(t, env, true)
	})
	if calls.Load() != 3 {
		t.Fatalf("real health invocations=%d", calls.Load())
	}
	before := calls.Load()
	for _, test := range []struct {
		name   string
		change func(map[string]string)
	}{
		{"partial explicit paths", func(e map[string]string) {
			delete(e, "MUXCORE_TLS_KEY")
			e["MUXCORE_TLS_DIR"] = filepath.Dir(provider.CertFile)
		}},
		{"missing CA", func(e map[string]string) { delete(e, "MUXCORE_TLS_CA") }},
		{"unreadable key", func(e map[string]string) { e["MUXCORE_TLS_KEY"] = "missing" }},
		{"borrowed admin CN", func(e map[string]string) {
			id := ca.Issue(t, "admin-ui")
			e["MUXCORE_TLS_CERT"] = id.CertFile
			e["MUXCORE_TLS_KEY"] = id.KeyFile
		}},
		{"unknown profile", func(e map[string]string) { e["MUXCORE_PROFILE"] = "typo" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := cloneEnv(explicit)
			test.change(env)
			invoke(t, env, false, "--origin", s.URL)
		})
	}
	for _, profile := range []string{"household", "staging"} {
		for _, alias := range []string{"MUXCORE_INSECURE_DISABLE_TLS", "MUXCORE_DEV_TLS_SKIP"} {
			t.Run(profile+alias, func(t *testing.T) {
				env := cloneEnv(explicit)
				env["MUXCORE_PROFILE"] = profile
				env[alias] = "true"
				invoke(t, env, false, "--origin", s.URL)
			})
		}
	}
	t.Run("no enrollment or storage", func(t *testing.T) {
		dir := t.TempDir()
		invoke(t, map[string]string{"MUXCORE_PROFILE": "household", "MUXCORE_TLS_DIR": dir, "MUXCORE_TLS_CA": ca.File, "MUXCORE_BOOTSTRAP_TOKEN": "fixture-bootstrap-secret", "MUXCORE_GRPC_ADDR": "127.0.0.1:1", "USERDATA_LOCAL_DATA_DIR": dir}, false, "--origin", s.URL)
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != 0 {
			t.Fatal("probe generated identity or opened storage")
		}
	})
	if calls.Load() != before {
		t.Fatal("bad configuration reached provider")
	}
	before = calls.Load()
	// Only the spellings core and the SDK honour select plaintext; anything else
	// leaves the household/unset-profile probe on its secure identity path.
	for name, env := range map[string]map[string]string{
		"legacy gRPC alias":           {"MUXCORE_PROFILE": "household", "MUXCORE_GRPC_INSECURE": "true"},
		"legacy gRPC alias, no prof.": {"MUXCORE_GRPC_INSECURE": "1"},
		"upper-case flag, no profile": {"MUXCORE_INSECURE_DISABLE_TLS": "TRUE"},
		"upper-case legacy flag":      {"MUXCORE_PROFILE": "household", "MUXCORE_DEV_TLS_SKIP": "True"},
	} {
		t.Run("secure despite "+name, func(t *testing.T) {
			e := cloneEnv(explicit)
			delete(e, "MUXCORE_PROFILE")
			for k, v := range env {
				e[k] = v
			}
			invoke(t, e, true, "--origin", s.URL)
		})
	}
	t.Run("daemon default identity directory", func(t *testing.T) {
		data := t.TempDir()
		for from, to := range map[string]string{provider.CertFile: "module.crt", provider.KeyFile: "module.key", ca.File: "ca.crt"} {
			raw, err := os.ReadFile(from)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(data, "mesh-id"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(data, "mesh-id", to), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		invoke(t, map[string]string{"MUXCORE_PROFILE": "household", "MUXCORE_DATA_DIR": data}, true, "--origin", s.URL)
	})
	t.Run("module ID override is unsupported", func(t *testing.T) {
		e := cloneEnv(explicit)
		e["MUXCORE_MODULE_ID"] = "renamed"
		invoke(t, e, false, "--origin", s.URL)
	})
	if got, want := calls.Load(), before+5; got != want {
		t.Fatalf("health invocations=%d want %d", got, want)
	}
	t.Run("wrong server", func(t *testing.T) {
		wrong := ca.Issue(t, "admin-ui")
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("probe trusted wrong provider") }))
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{wrong.Pair}}
		server.StartTLS()
		defer server.Close()
		invoke(t, explicit, false, "--origin", server.URL)
	})
	t.Run("explicit insecure dev", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/health" {
				t.Error("wrong dev path")
			}
			w.WriteHeader(200)
		}))
		defer server.Close()
		invoke(t, map[string]string{"MUXCORE_PROFILE": "dev", "MUXCORE_INSECURE_DISABLE_TLS": "true"}, true, "--origin", server.URL)
	})
}

func cloneEnv(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
