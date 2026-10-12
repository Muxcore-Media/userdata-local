package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Muxcore-Media/userdata-local/conditional"
)

func TestConditionalResponseCapIsSeparateAndExact(t *testing.T) {
	for _, tc := range []struct {
		name     string
		op       Operation
		size     int
		accepted bool
	}{
		{"legacy exact", GetUserdata, MaxResponseBytes, true},
		{"legacy metadata not allowed", GetUserdata, MaxResponseBytes + 1, false},
		{"v2 blob plus envelope", GetUserdataV2, MaxResponseBytes + 1, true},
		{"v2 exact cap", PutUserdataV2, conditional.MaxResponseBytes, true},
		{"v2 over cap", GetUserdataV2, conditional.MaxResponseBytes + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", tc.size)) }))
			defer s.Close()
			client := newClient(t, Config{Origin: s.URL, ModuleID: "media-ui", Profile: "dev", Insecure: true})
			resp, err := client.Do(context.Background(), tc.op, nil, nil)
			if tc.accepted {
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				raw, _ := io.ReadAll(resp.Body)
				if len(raw) != tc.size {
					t.Fatal("body truncated")
				}
			} else {
				var unavailable *UnavailableError
				if !errors.As(err, &unavailable) || unavailable.Reason != ReasonResponse || resp != nil {
					t.Fatalf("oversize became application response: %v", err)
				}
			}
		})
	}
}
