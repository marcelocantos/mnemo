// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/marcelocantos/mnemo/internal/store"
)

// Server listens on a Unix domain socket and dispatches RPC calls to the store.
type Server struct {
	store    *store.Store
	listener net.Listener
}

// NewServer creates an RPC server bound to the default socket path.
func NewServer(s *store.Store) (*Server, error) {
	return NewServerAt(s, SocketPath())
}

// NewServerAt creates an RPC server bound to the given socket path.
func NewServerAt(s *store.Store, sockPath string) (*Server, error) {
	// Remove stale socket file.
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}

	return &Server{store: s, listener: listener}, nil
}

// Serve accepts connections and handles them. Blocks until the listener is closed.
func (s *Server) Serve() error {
	slog.Info("RPC server listening", "socket", SocketPath())
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

// Close shuts down the listener.
func (s *Server) Close() error {
	return s.listener.Close()
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4<<20), 4<<20)
	enc := json.NewEncoder(conn)

	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			enc.Encode(Response{Error: fmt.Sprintf("invalid request: %v", err)})
			continue
		}

		result, err := s.dispatch(req)
		if err != nil {
			enc.Encode(Response{Error: err.Error()})
			continue
		}

		resultJSON, err := json.Marshal(result)
		if err != nil {
			enc.Encode(Response{Error: fmt.Sprintf("marshal result: %v", err)})
			continue
		}
		enc.Encode(Response{Result: resultJSON})
	}
}

// SearchParams matches the Search method signature.
type SearchParams struct {
	Query           string `json:"query"`
	Limit           int    `json:"limit"`
	SessionType     string `json:"session_type"`
	RepoFilter      string `json:"repo_filter"`
	ContextBefore   int    `json:"context_before"`
	ContextAfter    int    `json:"context_after"`
	SubstantiveOnly bool   `json:"substantive_only"`
}

// ListSessionsParams matches the ListSessions method signature.
type ListSessionsParams struct {
	SessionType    string `json:"session_type"`
	MinMessages    int    `json:"min_messages"`
	Limit          int    `json:"limit"`
	ProjectFilter  string `json:"project_filter"`
	RepoFilter     string `json:"repo_filter"`
	WorkTypeFilter string `json:"work_type_filter"`
}

// ReadSessionParams matches the ReadSession method signature.
type ReadSessionParams struct {
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
}

// QueryParams matches the Query method signature.
type QueryParams struct {
	Query string `json:"query"`
}

// ResolveNonceParams matches the ResolveNonce method signature.
type ResolveNonceParams struct {
	Nonce string `json:"nonce"`
}

func (s *Server) dispatch(req Request) (any, error) {
	switch req.Method {
	case "Search":
		var p SearchParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return s.store.Search(p.Query, p.Limit, p.SessionType, p.RepoFilter, p.ContextBefore, p.ContextAfter, p.SubstantiveOnly)

	case "ListSessions":
		var p ListSessionsParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return s.store.ListSessions(p.SessionType, p.MinMessages, p.Limit, p.ProjectFilter, p.RepoFilter, p.WorkTypeFilter)

	case "ReadSession":
		var p ReadSessionParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return s.store.ReadSession(p.SessionID, p.Role, p.Offset, p.Limit)

	case "Query":
		var p QueryParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return s.store.Query(p.Query)

	case "Stats":
		return s.store.Stats()

	case "ResolveNonce":
		var p ResolveNonceParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		sid, err := s.store.ResolveNonce(p.Nonce)
		if err != nil {
			return nil, err
		}
		return map[string]string{"session_id": sid}, nil

	default:
		return nil, fmt.Errorf("unknown method: %s", req.Method)
	}
}
