package server

import (
	"net/http"
	"sync/atomic"
)

// httpSrv is the listener Start is serving; ForceClose closes it without
// waiting for hijacked WebSocket handlers (unlike Shutdown).
var httpSrv atomic.Pointer[http.Server]

func (s *Server) trackHTTPServer(srv *http.Server) {
	httpSrv.Store(srv)
}

func (s *Server) untrackHTTPServer() {
	httpSrv.Store(nil)
}

// ForceClose closes tracked WebSockets and the HTTP listener immediately so
// Start can return on Ctrl+C / ctx cancel. Shutdown alone does not close
// hijacked WebSocket connections (see net/http.Server.Shutdown docs).
func (s *Server) ForceClose() {
	if s != nil {
		s.closeWSConns()
	}
	if srv := httpSrv.Load(); srv != nil {
		_ = srv.Close()
	}
}
