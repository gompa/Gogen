package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWriteJSONConcurrentCloseNoPanic(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		ws := newWSConn(conn)
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = ws.writeJSON(WSMessage{Type: "stream", Content: "x"})
			}()
		}
		time.Sleep(5 * time.Millisecond)
		ws.closeSend()
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = ws.writeJSON(WSMessage{Type: "stream", Content: "y"})
			}()
		}
		wg.Wait()
		select {
		case <-ws.done:
		case <-time.After(2 * time.Second):
			t.Error("writeLoop did not exit after closeSend")
		}
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[4:], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("handler timed out")
	}
}

func TestWriteJSONAfterCloseReturnsClosed(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	finished := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			finished <- err
			return
		}
		ws := newWSConn(conn)
		ws.closeSend()
		select {
		case <-ws.done:
		case <-time.After(2 * time.Second):
			finished <- errWSClosed
			return
		}
		if err := ws.writeJSON(WSMessage{Type: "ping"}); err != errWSClosed {
			finished <- err
			return
		}
		finished <- nil
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[4:], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case err := <-finished:
		if err == errWSClosed {
			t.Fatal("writeLoop did not exit after closeSend")
		}
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}
