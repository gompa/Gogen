package server

import (
	"net/http"
)

func (s *Server) trackHTTPServer(srv *http.Server) {
	s.httpSrv.Store(srv)
}

func (s *Server) untrackHTTPServer() {
	s.httpSrv.Store(nil)
}

// ForceClose closes tracked WebSockets and the HTTP listener immediately so
// Start can return on Ctrl+C / ctx cancel. Shutdown alone does not close
// hijacked WebSocket connections (see net/http.Server.Shutdown docs).
func (s *Server) ForceClose() {
	if s != nil {
		s.closeWSConns()
	}
	if srv := s.httpSrv.Load(); srv != nil {
		_ = srv.Close()
	}
}
