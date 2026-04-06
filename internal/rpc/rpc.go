// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package rpc provides the JSON-over-UDS protocol between the mnemo
// stdio proxy and the persistent serve process.
package rpc

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// SocketPath returns the default Unix domain socket path.
func SocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mnemo", "mnemo.sock")
}

// BinaryHash returns the SHA-256 hash of the running executable.
// Cached after first call.
func BinaryHash() string {
	binaryHashOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		data, err := os.ReadFile(exe)
		if err != nil {
			return
		}
		h := sha256.Sum256(data)
		binaryHashValue = hex.EncodeToString(h[:])
	})
	return binaryHashValue
}

var (
	binaryHashOnce  sync.Once
	binaryHashValue string
)

// Handshake is the first message sent by the client after connecting.
type Handshake struct {
	BinaryHash string `json:"binary_hash"`
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
type Client struct {
	conn    net.Conn
	enc     *json.Encoder
	scanner *bufio.Scanner
}

// Dial connects to the mnemo serve process at the default socket path.
func Dial() (*Client, error) {
	return DialAt(SocketPath())
}

// DialAt connects to the mnemo serve process at the given socket path.
func DialAt(sockPath string) (*Client, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to mnemo serve (is it running?): %w", err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4<<20), 4<<20) // 4MB line buffer
	enc := json.NewEncoder(conn)

	// Send handshake with binary hash.
	if err := enc.Encode(Handshake{BinaryHash: BinaryHash()}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake send: %w", err)
	}

	// Read handshake response.
	if !scanner.Scan() {
		conn.Close()
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("handshake recv: %w", err)
		}
		return nil, fmt.Errorf("connection closed during handshake")
	}
	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake decode: %w", err)
	}
	if resp.Error != "" {
		conn.Close()
		return nil, fmt.Errorf("%s", resp.Error)
	}

	return &Client{
		conn:    conn,
		enc:     enc,
		scanner: scanner,
	}, nil
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Call sends a request and waits for the response.
func (c *Client) Call(method string, params any) (json.RawMessage, error) {
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
