// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package mcpbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// DaemonConfig configures a daemon RPC server.
type DaemonConfig struct {
	SocketPath string     // Required. Unix domain socket path.
	Tools      []mcp.Tool // MCP tool definitions to serve via ListTools.
	Handler    ToolHandler // Handles CallTool RPCs.

	// ExtraMethods registers additional RPC methods beyond ListTools
	// and CallTool. Each function receives raw JSON params and returns
	// a result to be JSON-marshaled.
	ExtraMethods map[string]MethodFunc
}

// Server listens on a Unix domain socket and dispatches RPC calls.
type Server struct {
	tools        []mcp.Tool
	handler      ToolHandler
	extraMethods map[string]MethodFunc
	listener     net.Listener

	mu    sync.Mutex
	conns []net.Conn
}

// ServerOption configures a Server after creation.
type ServerOption func(*Server)

// WithToolDefs overrides the tool definitions.
// Useful for testing tool list changes across daemon restarts.
func WithToolDefs(defs []mcp.Tool) ServerOption {
	return func(s *Server) { s.tools = defs }
}

// NewServer creates a daemon RPC server bound to the configured socket path.
func NewServer(cfg DaemonConfig, opts ...ServerOption) (*Server, error) {
	// Remove stale socket file.
	os.Remove(cfg.SocketPath)

	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.SocketPath, err)
	}

	srv := &Server{
		tools:        cfg.Tools,
		handler:      cfg.Handler,
		extraMethods: cfg.ExtraMethods,
		listener:     listener,
	}
	for _, opt := range opts {
		opt(srv)
	}
	return srv, nil
}

// Serve accepts connections and handles them. Blocks until the listener
// is closed or ctx is cancelled.
func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

// Close shuts down the listener and all active connections.
func (s *Server) Close() error {
	err := s.listener.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
	s.mu.Unlock()
	return err
}

func (s *Server) trackConn(conn net.Conn) {
	s.mu.Lock()
	s.conns = append(s.conns, conn)
	s.mu.Unlock()
}

func (s *Server) handleConn(conn net.Conn) {
	s.trackConn(conn)
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4<<20), 4<<20)
	enc := json.NewEncoder(conn)

	// Read and validate handshake.
	if !scanner.Scan() {
		return
	}
	var hs Handshake
	if err := json.Unmarshal(scanner.Bytes(), &hs); err != nil {
		enc.Encode(Response{Error: fmt.Sprintf("invalid handshake: %v", err)})
		return
	}
	if hs.ProtocolVersion != ProtocolVersion {
		enc.Encode(Response{Error: fmt.Sprintf(
			"protocol version mismatch: proxy=%d, daemon=%d — restart the daemon",
			hs.ProtocolVersion, ProtocolVersion)})
		return
	}
	enc.Encode(Response{Result: []byte(`"ok"`)})

	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			enc.Encode(Response{Error: fmt.Sprintf("invalid request: %v", err)})
			continue
		}

		start := time.Now()
		result, err := s.dispatch(req)
		dur := time.Since(start)

		if err != nil {
			slog.Warn("rpc", "method", req.Method, "dur", dur, "err", err)
			enc.Encode(Response{Error: err.Error()})
			continue
		}

		logLevel := slog.LevelDebug
		if dur >= 100*time.Millisecond {
			logLevel = slog.LevelInfo
		}
		if dur >= time.Second {
			logLevel = slog.LevelWarn
		}
		slog.Log(context.Background(), logLevel, "rpc", "method", req.Method, "dur", dur)

		resultJSON, err := json.Marshal(result)
		if err != nil {
			enc.Encode(Response{Error: fmt.Sprintf("marshal result: %v", err)})
			continue
		}
		enc.Encode(Response{Result: resultJSON})
	}
}

func (s *Server) dispatch(req Request) (any, error) {
	switch req.Method {
	case "ListTools":
		return s.tools, nil

	case "CallTool":
		var p callToolParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		text, isErr, err := s.handler.Call(p.Name, p.Args)
		if err != nil {
			return nil, err
		}
		return CallResult{Text: text, IsError: isErr}, nil

	default:
		if fn, ok := s.extraMethods[req.Method]; ok {
			return fn(req.Params)
		}
		return nil, fmt.Errorf("unknown method: %s", req.Method)
	}
}
