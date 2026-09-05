package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	userdatav1 "github.com/Muxcore-Media/userdata-local/proto/gen/muxcore/userdata/v1"
)

// Server implements UserDataService and HTTP handlers.
type Server struct {
	userdatav1.UnimplementedUserDataServiceServer
	store *store.Store
	guard *auth.Guard
}

func New(st *store.Store, guard *auth.Guard) *Server {
	return &Server{store: st, guard: guard}
}

func (s *Server) RegisterWithGRPC(srv *grpc.Server) {
	userdatav1.RegisterUserDataServiceServer(srv, s)
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/userdata", s.handleUserdata)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

func (s *Server) Get(ctx context.Context, req *userdatav1.GetRequest) (*userdatav1.GetResponse, error) {
	if err := s.guard.AuthorizeGRPC(ctx, req.GetUserId()); err != nil {
		return nil, err
	}
	blob, revision, err := s.store.Get(req.GetUserId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		return nil, status.Error(codes.Internal, "encode blob")
	}
	return &userdatav1.GetResponse{Json: raw, Revision: revision}, nil
}

func (s *Server) Put(ctx context.Context, req *userdatav1.PutRequest) (*userdatav1.PutResponse, error) {
	if err := s.guard.AuthorizeGRPC(ctx, req.GetUserId()); err != nil {
		return nil, err
	}
	incoming, err := store.ParseBlob(req.GetJson())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid json blob")
	}
	blob, revision, err := s.store.Put(req.GetUserId(), incoming)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		return nil, status.Error(codes.Internal, "encode blob")
	}
	return &userdatav1.PutResponse{Json: raw, Revision: revision}, nil
}

func (s *Server) ListContinueWatching(ctx context.Context, req *userdatav1.ListContinueWatchingRequest) (*userdatav1.ListContinueWatchingResponse, error) {
	if err := s.guard.AuthorizeGRPC(ctx, req.GetUserId()); err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 24
	}
	items, err := s.store.ListContinueWatching(req.GetUserId(), limit)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, status.Error(codes.Internal, "encode items")
	}
	return &userdatav1.ListContinueWatchingResponse{Json: raw}, nil
}

func (s *Server) handleUserdata(w http.ResponseWriter, r *http.Request) {
	_, userID, err := s.guard.AuthenticateHTTP(r)
	if err != nil {
		code := http.StatusUnauthorized
		if strings.Contains(err.Error(), "does not match") || strings.Contains(err.Error(), "permission") {
			code = http.StatusForbidden
		}
		http.Error(w, err.Error(), code)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.writeBlob(w, userID, nil)
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		s.writeBlob(w, userID, body)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) writeBlob(w http.ResponseWriter, userID string, body []byte) {
	var blobJSON []byte
	if len(body) == 0 {
		blob, _, err := s.store.Get(userID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		blobJSON, err = json.Marshal(blob)
		if err != nil {
			http.Error(w, "encode blob", http.StatusInternalServerError)
			return
		}
	} else {
		incoming, err := store.ParseBlob(body)
		if err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		blob, _, err := s.store.Put(userID, incoming)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		blobJSON, err = json.Marshal(blob)
		if err != nil {
			http.Error(w, "encode blob", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(blobJSON)
}
