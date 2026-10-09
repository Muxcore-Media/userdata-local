package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	"github.com/Muxcore-Media/userdata-local/parental"
)

func (s *Server) handleParentalPolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		writePolicyError(w, http.StatusMethodNotAllowed, "policy.method_not_allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	scope, actor, err := s.guard.AuthorizePolicyHTTP(r, r.Method == http.MethodPut)
	if err != nil {
		status, code := http.StatusServiceUnavailable, "policy.identity_unavailable"
		switch {
		case errors.Is(err, auth.ErrPolicyUnauthenticated):
			status, code = http.StatusUnauthorized, "policy.unauthenticated"
		case errors.Is(err, auth.ErrPolicyForbidden):
			status, code = http.StatusForbidden, "policy.forbidden"
		case errors.Is(err, auth.ErrPolicyTargetNotFound):
			status, code = http.StatusNotFound, "policy.account_not_found"
		case errors.Is(err, auth.ErrPolicyInvalidTarget):
			status, code = http.StatusBadRequest, "policy.invalid_target"
		}
		writePolicyError(w, status, code)
		return
	}
	// There are no query selectors for identity, tenant, profile, or policy.
	if r.URL.RawQuery != "" {
		writePolicyError(w, http.StatusBadRequest, "policy.invalid_query")
		return
	}
	var document parental.Document
	if r.Method == http.MethodGet {
		document, err = s.store.GetParentalPolicy(ctx, scope)
	} else {
		body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, parental.MaxBodyBytes))
		if readErr != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(readErr, &tooLarge) {
				writePolicyError(w, http.StatusRequestEntityTooLarge, "policy.too_large")
			} else {
				writePolicyError(w, http.StatusBadRequest, "policy.invalid_body")
			}
			return
		}
		update, decodeErr := parental.DecodeUpdate(body)
		if decodeErr != nil {
			writePolicyError(w, http.StatusBadRequest, "policy.invalid_body")
			return
		}
		document, err = s.store.PutParentalPolicy(ctx, scope, actor, update)
	}
	if errors.Is(err, store.ErrUserErased) {
		writePolicyError(w, http.StatusGone, CodePolicyAccountErased)
		return
	}
	if errors.Is(err, store.ErrPolicyConflict) {
		writePolicyError(w, http.StatusConflict, "policy.revision_conflict")
		return
	}
	if err != nil {
		writePolicyError(w, http.StatusServiceUnavailable, "policy.storage_unavailable")
		return
	}
	_ = json.NewEncoder(w).Encode(document)
}

func writePolicyError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}
