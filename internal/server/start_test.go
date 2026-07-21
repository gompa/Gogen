package server

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestStartShutsDownOnCancel(t *testing.T) {
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

	// Wait until the listener is up.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server did not start listening: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}
