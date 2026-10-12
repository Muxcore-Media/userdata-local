package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Muxcore-Media/userdata-local/conditional"
	"github.com/Muxcore-Media/userdata-local/internal/store"
)

func (s *Server) handleConditional(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		writeConditionalError(w, conditional.MethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	userID, err := s.guard.AuthorizeConditionalHTTP(r)
	if err != nil {
		writeConditionalError(w, err)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeConditionalError(w, conditional.InvalidQuery)
		return
	}
	var document conditional.Document
	if r.Method == http.MethodGet {
		document, err = s.store.GetConditional(ctx, userID)
	} else {
		body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, conditional.MaxRequestBytes))
		if readErr != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(readErr, &tooLarge) {
				writeConditionalError(w, conditional.TooLarge)
			} else {
				writeConditionalError(w, conditional.InvalidBody)
			}
			return
		}
		update, decodeErr := conditional.DecodeUpdate(body)
		if decodeErr != nil {
			writeConditionalError(w, decodeErr)
			return
		}
		document, err = s.store.PutConditional(ctx, userID, update)
	}
	if err != nil {
		writeConditionalError(w, err)
		return
	}
	raw, err := conditional.EncodeDocument(document)
	if err != nil {
		// The store checked this exact encoding before commit. Never reinterpret
		// an unexpected post-commit encode failure as a known refusal.
		writeConditionalError(w, conditional.StorageUnavailable)
		return
	}
	_, _ = w.Write(raw)
}

func writeConditionalError(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, string(conditional.StorageUnavailable)
	var known conditional.Error
	if errors.As(err, &known) {
		switch known {
		case conditional.ProtocolUnsupported, conditional.MutationUnsupported, conditional.InvalidBody, conditional.InvalidPrecondition, conditional.InvalidQuery:
			status, code = http.StatusBadRequest, string(known)
		case conditional.PreconditionRequired:
			status, code = http.StatusPreconditionRequired, string(known)
		case conditional.Unauthenticated:
			status, code = http.StatusUnauthorized, string(known)
		case conditional.Forbidden, conditional.ScopeUnsupported:
			status, code = http.StatusForbidden, string(known)
		case conditional.MethodNotAllowed:
			status, code = http.StatusMethodNotAllowed, string(known)
		case conditional.AuthorityConflict, conditional.RevisionConflict:
			status, code = http.StatusConflict, string(known)
		case conditional.TooLarge:
			status, code = http.StatusRequestEntityTooLarge, string(known)
		case conditional.IdentityUnavailable, conditional.RevisionExhausted, conditional.StorageUnavailable:
			code = string(known)
		}
	}
	if errors.Is(err, store.ErrUserErased) {
		status, code = http.StatusGone, CodeUserdataAccountErased
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}
