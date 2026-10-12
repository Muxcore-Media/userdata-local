package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Muxcore-Media/userdata-local/conditional"
)

func TestConditionalV3FixedOperationsAndResponseBounds(t *testing.T) {
	for _, tc := range []struct {
		op       Operation
		method   string
		size     int
		accepted bool
	}{
		{GetUserdataV3, "GET", conditional.MaxResponseBytes, true},
		{PutUserdataV3, "PUT", conditional.MaxResponseBytes, true},
		{GetUserdataV3, "GET", conditional.MaxResponseBytes + 1, false},
		{PutUserdataV3, "PUT", conditional.MaxResponseBytes + 1, false},
	} {
		t.Run(tc.method+"/"+strconv.Itoa(tc.size), func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != tc.method || r.URL.Path != conditional.PathV3 || r.URL.RawQuery != "" {
					t.Errorf("unexpected dispatch %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("X-MuxCore-User-Id") != "alice" {
					t.Error("identity changed")
				}
				_, _ = io.WriteString(w, strings.Repeat("x", tc.size))
			}))
			defer s.Close()
			client := newClient(t, Config{Origin: s.URL, ModuleID: "media-ui", Profile: "dev", Insecure: true})
			resp, err := client.Do(context.Background(), tc.op, http.Header{"Authorization": {"Bearer fixture"}, "X-MuxCore-User-Id": {"alice"}}, nil)
			if tc.accepted {
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if len(raw) != tc.size {
					t.Fatal("truncated response")
				}
			} else {
				var unavailable *UnavailableError
				if resp != nil || !errors.As(err, &unavailable) || unavailable.Reason != ReasonResponse {
					t.Fatalf("oversize result: %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("retried dispatch")
			}
		})
	}
	if GetUserdataV2 != 7 || PutUserdataV2 != 8 {
		t.Fatal("existing operation identifiers changed")
	}
}

func TestConditionalV3DoesNotFollowRedirectOrHideAdmissionRefusal(t *testing.T) {
	for _, op := range []Operation{GetUserdataV3, PutUserdataV3} {
		var targets atomic.Int64
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targets.Add(1) }))
		defer target.Close()
		for _, status := range []int{301, 302, 303, 307, 308, 403} {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == 403 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"code":"userdata.module_forbidden"}`)
					return
				}
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
			}))
			client := newClient(t, Config{Origin: source.URL, ModuleID: "media-ui", Profile: "dev", Insecure: true})
			resp, err := client.Do(context.Background(), op, http.Header{"Authorization": {"Bearer fixture"}}, strings.NewReader("{}"))
			var unavailable *UnavailableError
			if resp != nil || !errors.As(err, &unavailable) {
				t.Fatalf("unexpected response %v %v", resp, err)
			}
			if status == 403 && unavailable.Reason != ReasonModuleForbidden {
				t.Fatalf("lost admission reason: %v", err)
			}
			if status != 403 && unavailable.Reason != ReasonRedirect {
				t.Fatalf("redirect lost uncertainty reason: %v", err)
			}
			source.Close()
		}
		if targets.Load() != 0 {
			t.Fatal("redirect leaked dispatch")
		}
	}
}

func TestConditionalV3PreservesApplicationRefusalsWithoutRetry(t *testing.T) {
	for _, op := range []Operation{GetUserdataV3, PutUserdataV3} {
		for status, code := range map[int]string{400: "userdata.invalid_body", 401: "userdata.unauthenticated", 403: "userdata.forbidden", 409: "userdata.revision_conflict", 410: "userdata.account_erased", 413: "userdata.too_large", 428: "userdata.precondition_required", 503: "userdata.storage_unavailable"} {
			var calls atomic.Int64
			body := `{"code":"` + code + `"}`
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			client := newClient(t, Config{Origin: s.URL, ModuleID: "media-ui", Profile: "dev", Insecure: true})
			resp, err := client.Do(context.Background(), op, nil, strings.NewReader("{}"))
			if err != nil {
				t.Fatal("application refusal hidden as transport error", err)
			}
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			s.Close()
			if err != nil || resp.StatusCode != status || string(raw) != body || calls.Load() != 1 {
				t.Fatalf("refusal changed/retried: %d %s calls=%d %v", resp.StatusCode, raw, calls.Load(), err)
			}
		}
	}
}
