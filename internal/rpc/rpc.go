// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package rpc provides the JSON-over-UDS protocol between the mnemo
// stdio proxy and the persistent serve process.
package rpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// ProtocolVersion is bumped only when the RPC wire format between the
// proxy and daemon changes (new/renamed methods, changed param types).
// It is independent of the MCP protocol and the application version.
const ProtocolVersion = 1

// SocketPath returns the default Unix domain socket path.
func SocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mnemo", "mnemo.sock")
}

// Handshake is the first message sent by the client after connecting.
type Handshake struct {
	ProtocolVersion int `json:"protocol_version"`
}

// Request is a JSON-RPC-like request sent over the UDS.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Response is a JSON-RPC-like response sent over the UDS.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Client connects to the mnemo serve process over UDS.
// It automatically reconnects on broken pipe errors.
type Client struct {
	mu       sync.Mutex
	sockPath string
	conn     net.Conn
	enc      *json.Encoder
	scanner  *bufio.Scanner
}

// Dial connects to the mnemo serve process at the default socket path.
func Dial() (*Client, error) {
	return DialAt(SocketPath())
}

// DialAt connects to the mnemo serve process at the given socket path.
func DialAt(sockPath string) (*Client, error) {
	c := &Client{sockPath: sockPath}
	if err := c.connect(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) connect() error {
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return fmt.Errorf("cannot connect to mnemo serve (is it running?): %w", err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4<<20), 4<<20) // 4MB line buffer
	enc := json.NewEncoder(conn)

	// Send handshake with protocol version.
	if err := enc.Encode(Handshake{ProtocolVersion: ProtocolVersion}); err != nil {
		conn.Close()
		return fmt.Errorf("handshake send: %w", err)
	}

	// Read handshake response.
	if !scanner.Scan() {
		conn.Close()
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("handshake recv: %w", err)
		}
		return fmt.Errorf("connection closed during handshake")
	}
	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		conn.Close()
		return fmt.Errorf("handshake decode: %w", err)
	}
	if resp.Error != "" {
		conn.Close()
		return fmt.Errorf("%s", resp.Error)
	}

	c.conn = conn
	c.enc = enc
	c.scanner = scanner
	return nil
}

// Close closes the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// isBrokenPipe returns true if the error indicates a broken connection.
func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	// Also check for "connection closed" from scanner failures.
	msg := err.Error()
	return msg == "connection closed" ||
		// net.OpError wraps these
		errors.Is(err, net.ErrClosed)
}

// Call sends a request and waits for the response.
// On broken pipe, it reconnects once and retries.
func (c *Client) Call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	result, err := c.callLocked(method, params)
	if err != nil && isBrokenPipe(err) {
		// Connection lost — try to reconnect once.
		if c.conn != nil {
			c.conn.Close()
		}
		if reconnErr := c.connect(); reconnErr != nil {
			return nil, fmt.Errorf("reconnect failed after %w: %v", err, reconnErr)
		}
		return c.callLocked(method, params)
	}
	return result, err
}

func (c *Client) callLocked(method string, params any) (json.RawMessage, error) {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if err := c.enc.Encode(Request{Method: method, Params: paramsJSON}); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("recv: %w", err)
		}
		return nil, fmt.Errorf("connection closed")
	}
	var resp Response
	if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Result, nil
}
