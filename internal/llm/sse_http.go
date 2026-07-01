package llm

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// streamReadIdleTimeout is the per-read deadline for SSE response bodies.
// llama.cpp often stops sending without closing the connection or sending [DONE];
// this bounds how long we block waiting for the next byte. Set
// GOGEN_STREAM_IDLE_TIMEOUT=0 to disable (wait indefinitely).
func streamReadIdleTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("GOGEN_STREAM_IDLE_TIMEOUT"))
	if raw == "" {
		return 10 * time.Minute
	}
	if raw == "0" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "false") {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 10 * time.Minute
	}
	return d
}

type idleTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleTimeoutConn) Read(b []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Read(b)
}

// newSSEHTTPClient returns an HTTP client tuned for Server-Sent Events.
// http.DefaultClient advertises Accept-Encoding: gzip, which makes many
// backends (including llama.cpp) compress the stream; the gzip reader then
// delivers tokens in bursts instead of incrementally.
func newSSEHTTPClient() *http.Client {
	idle := streamReadIdleTimeout()
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				if idle > 0 {
					return &idleTimeoutConn{Conn: conn, timeout: idle}, nil
				}
				return conn, nil
			},
		},
	}
}
