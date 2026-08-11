package server

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var errWSClosed = errors.New("websocket closed")

type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex

	sendQ chan WSMessage
	quit  chan struct{} // closed by closeSend to stop writers + writeLoop
	done  chan struct{} // closed when writeLoop exits, so writeJSON fails fast
	once  sync.Once
}

const (
	wsSendQueueSize   = 4096
	wsPingInterval    = 30 * time.Second
	wsWriteTimeout    = 30 * time.Second
	wsReadTimeout     = 60 * time.Second
	wsTurnAcquireWait = 150 * time.Millisecond
	// UI cancel: wait briefly for StreamProcessInput to finish cancel repair.
	wsStreamDrainWait = 2 * time.Second
)

// ── Debug-only transport instruments (live harness) ─────────────────────
// The jsdom harness (tmp/live_stall_detach.js) cannot create real TCP
// backpressure — its WebSocket drains instantly — so these env vars inject
// the stall server-side. Every knob is off by default; with none set the
// write path is byte-for-byte unchanged.
//
//	GOGEN_WS_SENDQ_SIZE         override wsSendQueueSize (default 4096). A
//	                            tiny queue under a stalling writer overflows
//	                            quickly, so enqueueJSON's 5s timeout fires
//	                            and broadcast detaches the socket — the
//	                            "stops mid-turn" candidate (DEBUG_PLAN.md C).
//	GOGEN_WS_STALL_MS           sleep this long before every data write once
//	                            stalling has begun (simulated slow client).
//	GOGEN_WS_STALL_AFTER_MS     begin stalling only once the connection has
//	                            been alive this long, so the harness can set
//	                            up panes and turns at normal speed first.
//	GOGEN_WS_STALL_FOR_MS       end stalling this long after it began, so
//	                            the writer drains the queue and a re-attach
//	                            can recover; 0/unset = stall for the
//	                            connection's lifetime.
//	GOGEN_WS_STALL_FIRST_CONN=1 stall only the first connection, so a
//	                            reconnect (e.g. a stall ≥ wsWriteTimeout that
//	                            kills the writer) is clean.
type wsDebugConfig struct {
	sendQSize  int
	stall      time.Duration
	stallAfter time.Duration
	stallFor   time.Duration
	firstConn  bool
}

var (
	wsDebugOnce  sync.Once
	wsDebugCfg   wsDebugConfig
	wsDebugConns atomic.Uint64 // debug-only: connection ordinal for GOGEN_WS_STALL_FIRST_CONN
)

// wsDebugConfigLoad reads the GOGEN_WS_STALL_*/GOGEN_WS_SENDQ_SIZE env vars
// once per process (they cannot change mid-run) and returns the config.
func wsDebugConfigLoad() wsDebugConfig {
	wsDebugOnce.Do(func() {
		cfg := wsDebugConfig{}
		if v := strings.TrimSpace(os.Getenv("GOGEN_WS_SENDQ_SIZE")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.sendQSize = n
			}
		}
		ms := func(name string) time.Duration {
			if v := strings.TrimSpace(os.Getenv(name)); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					return time.Duration(n) * time.Millisecond
				}
			}
			return 0
		}
		cfg.stall = ms("GOGEN_WS_STALL_MS")
		cfg.stallAfter = ms("GOGEN_WS_STALL_AFTER_MS")
		cfg.stallFor = ms("GOGEN_WS_STALL_FOR_MS")
		cfg.firstConn = strings.TrimSpace(os.Getenv("GOGEN_WS_STALL_FIRST_CONN")) == "1"
		wsDebugCfg = cfg
	})
	return wsDebugCfg
}

// drainStreamErr waits for the stream goroutine to signal exit.
// Returns true if the signal arrived, false on timeout (caller should keep ch).
func drainStreamErr(ch chan error) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	case <-time.After(wsStreamDrainWait):
		log.Printf("warning: timed out waiting for stream goroutine to exit")
		return false
	}
}

func newWSConn(conn *websocket.Conn) *wsConn {
	cfg := wsDebugConfigLoad()
	qsize := wsSendQueueSize
	if cfg.sendQSize > 0 {
		qsize = cfg.sendQSize // GOGEN_WS_SENDQ_SIZE: debug-only tiny queue
	}
	w := &wsConn{
		conn:  conn,
		sendQ: make(chan WSMessage, qsize),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go w.writeLoop()
	return w
}

func (w *wsConn) writeLoop() {
	// Closing the conn on exit is critical: it tears down the read loop (so
	// HandleWS cleans up) AND makes the browser fire onclose so it reconnects.
	// Without this, a single transient write error kills the writer silently
	// while the LLM keeps "sending" into a dead queue and the UI freezes.
	defer w.conn.Close()
	defer close(w.done)
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	// Debug-only backpressure (tmp/live_stall_detach.js): sleep before each
	// data write inside the [stallStart, stallEnd] window to simulate a
	// client that cannot drain its socket. The write deadline is set BEFORE
	// the sleep, so a stall ≥ wsWriteTimeout trips it and the writer dies
	// (socket drop → browser reconnects), while a smaller stall merely makes
	// the writer lag the stream and the send queue fills (detach via
	// enqueueJSON's 5s timeout). Zero stall = the path is untouched.
	cfg := wsDebugConfigLoad()
	stallStart := time.Now().Add(cfg.stallAfter)
	stallEnd := stallStart.Add(cfg.stallFor)
	if cfg.stallFor == 0 {
		stallEnd = stallStart.Add(24 * time.Hour) // unset = stall for the connection's lifetime
	}
	stallEnabled := cfg.stall > 0
	if cfg.firstConn && wsDebugConns.Add(1) > 1 {
		stallEnabled = false // only the first connection stalls; reconnects are clean
	}
	for {
		select {
		case <-w.quit:
			return
		case msg := <-w.sendQ:
			w.mu.Lock()
			if err := w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
				w.mu.Unlock()
				log.Printf("websocket set write deadline: %v", err)
				return
			}
			if stallEnabled && time.Now().After(stallStart) && time.Now().Before(stallEnd) {
				time.Sleep(cfg.stall)
			}
			err := w.conn.WriteJSON(msg)
			w.mu.Unlock()
			if err != nil {
				return
			}
		case <-ticker.C:
			// Pings detect half-open connections (e.g. NAT/proxy idle
			// timeouts, backgrounded tabs) that pass write deadlines but
			// never reach the browser. A failed ping kills the writer,
			// triggering teardown + reconnect via the deferred Close.
			w.mu.Lock()
			if err := w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
				w.mu.Unlock()
				log.Printf("websocket set write deadline: %v", err)
				return
			}
			err := w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			w.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (w *wsConn) closeSend() {
	w.once.Do(func() {
		// Signal quit instead of closing sendQ so concurrent writeJSON
		// sends cannot panic on a closed channel.
		close(w.quit)
	})
}

func (w *wsConn) writeJSON(v WSMessage) error {
	err := w.enqueueJSON(v)
	if err != nil && !errors.Is(err, errWSClosed) {
		log.Printf("websocket write (%s): %v", v.Type, err)
	}
	return err
}

func (w *wsConn) enqueueJSON(v WSMessage) error {
	if w == nil || w.conn == nil {
		return errWSClosed
	}
	select {
	case <-w.quit:
		return errWSClosed
	case <-w.done:
		return errWSClosed
	default:
	}
	select {
	case w.sendQ <- v:
		return nil
	case <-w.quit:
		return errWSClosed
	case <-w.done:
		return errWSClosed
	default:
		// Queue full: block briefly rather than stall the LLM stream reader forever.
		select {
		case w.sendQ <- v:
			return nil
		case <-w.quit:
			return errWSClosed
		case <-w.done:
			return errWSClosed
		case <-time.After(5 * time.Second):
			return fmt.Errorf("websocket send queue full")
		}
	}
}

// registerWSConn adds conn to the tracked set so the server can close it
// on graceful shutdown.
func (s *Server) registerWSConn(conn *websocket.Conn) {
	s.wsConnsMu.Lock()
	s.wsConns = append(s.wsConns, conn)
	s.wsConnsMu.Unlock()
}

// unregisterWSConn removes conn from the tracked set so shutdown does not
// close a connection that has already been cleaned up.
func (s *Server) unregisterWSConn(conn *websocket.Conn) {
	s.wsConnsMu.Lock()
	defer s.wsConnsMu.Unlock()
	for i, c := range s.wsConns {
		if c == conn {
			s.wsConns = append(s.wsConns[:i], s.wsConns[i+1:]...)
			return
		}
	}
}

// closeWSConns force-closes all tracked WebSocket connections. Safe to call
// concurrently with register/unregister. Never blocks on a single conn.
func (s *Server) closeWSConns() {
	s.wsConnsMu.Lock()
	conns := s.wsConns
	s.wsConns = nil
	s.wsConnsMu.Unlock()
	now := time.Now()
	for _, conn := range conns {
		if conn == nil {
			continue
		}
		c := conn
		go func() {
			_ = c.SetReadDeadline(now)
			_ = c.SetWriteDeadline(now)
			_ = c.Close()
		}()
	}
}
