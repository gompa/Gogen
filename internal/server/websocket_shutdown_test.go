package server

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestTrackedWebSocketConnectionsClosedOnShutdown(t *testing.T) {
	// Verify the closeWSConns mechanism: register connections and confirm
	// they get cleared when closeWSConns is called.
	s := &Server{}

	// Verify unregister removes from the slice.
	s.registerWSConn(nil) // index 0
	s.registerWSConn(nil) // index 1
	if len(s.wsConns) != 2 {
		t.Fatalf("expected 2 tracked conns, got %d", len(s.wsConns))
	}
	s.unregisterWSConn(nil)
	if len(s.wsConns) != 1 {
		t.Fatalf("expected 1 tracked conn after unregister, got %d", len(s.wsConns))
	}

	// Verify closeWSConns clears all.
	s.closeWSConns()
	if len(s.wsConns) != 0 {
		t.Fatalf("expected 0 tracked conns after closeWSConns, got %d", len(s.wsConns))
	}
}

func TestStartShutsDownWithWebSocket(t *testing.T) {
	// Integration-style test: start the server, open a TCP connection,
	// cancel, and verify Start returns promptly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	s := &Server{}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx, addr)
	}()

	// Wait for the server to start listening.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, dialErr := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if dialErr == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server did not start listening: %v", dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Simulate what HandleWS does: add a tracked connection.
	s.registerWSConn(nil)

	// Now shut down.
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return within 3s after cancel")
	}

	// After shutdown, wsConns should have been cleared by closeWSConns.
	if len(s.wsConns) != 0 {
		t.Fatalf("expected 0 tracked conns after shutdown, got %d", len(s.wsConns))
	}
}

// TestConcurrentRegisterUnregister verifies thread safety of wsConn tracking.
func TestConcurrentRegisterUnregister(t *testing.T) {
	s := &Server{}
	var wg sync.WaitGroup

	// Concurrently register and unregister connections.
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.registerWSConn(nil)
			s.unregisterWSConn(nil)
		}()
	}

	// Concurrently close all connections.
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.closeWSConns()
	}()

	wg.Wait()
	// After all operations, wsConns should be empty (closed by closeWSConns).
	if len(s.wsConns) != 0 {
		t.Fatalf("expected 0 tracked conns after concurrent ops, got %d", len(s.wsConns))
	}
}
