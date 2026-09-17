package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxDNSPacket       = 65535
	readTimeout        = 5 * time.Second
	writeTimeout       = 5 * time.Second
	defaultMaxBody     = maxDNSPacket
	defaultMaxInflight = 32
	defaultMaxConns    = 128
	// Bounds per-connection header buffering. DoH GET/POST requests never
	// need more than a few hundred bytes of headers; capping this well below
	// net/http's 1 MiB default matters on a memory-constrained instance
	// where many slow or hostile clients could otherwise hold larger buffers
	// open simultaneously.
	maxHeaderBytes = 8 << 10 // 8 KiB
)

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// limitListener caps simultaneously open TCP connections. The cap is kept
// modest because each connection consumes kernel and Go runtime resources.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func newLimitListener(l net.Listener, maxConns int) net.Listener {
	return &limitListener{Listener: l, sem: make(chan struct{}, maxConns)}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitListenerConn{Conn: c, release: l.sem}, nil
}

type limitListenerConn struct {
	net.Conn
	release chan struct{}
	once    sync.Once
}

func (c *limitListenerConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.release })
	return err
}

func decodeQuery(v string) ([]byte, error) {
	encodings := [...]*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	}
	var lastErr error
	for _, enc := range encodings {
		b, err := enc.DecodeString(v)
		if err == nil {
			return b, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// watchCancel closes conn early if ctx is cancelled (the DoH client went
// away) before the exchange finishes on its own. This frees the local socket
// and unblocks any pending Read/Write promptly instead of holding it for the
// full readTimeout, which matters when many short-lived goroutines are the
// difference between headroom and pressure on a 0.25 vCPU instance.
func watchCancel(ctx context.Context, conn net.Conn) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func dnsExchangeUDP(ctx context.Context, addr string, q []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer watchCancel(ctx, conn)()

	_ = conn.SetDeadline(time.Now().Add(readTimeout))
	if _, err := conn.Write(q); err != nil {
		return nil, err
	}

	buf := make([]byte, maxDNSPacket)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	if n < 12 {
		return nil, errors.New("short DNS response")
	}
	return buf[:n], nil
}

func dnsExchangeTCP(ctx context.Context, addr string, q []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer watchCancel(ctx, conn)()
	_ = conn.SetDeadline(time.Now().Add(readTimeout))

	frame := []byte{byte(len(q) >> 8), byte(len(q))}
	frame = append(frame, q...)
	if _, err := conn.Write(frame); err != nil {
		return nil, err
	}

	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	n := int(hdr[0])<<8 | int(hdr[1])
	if n <= 0 || n > maxDNSPacket {
		return nil, errors.New("invalid TCP DNS response size")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	if len(buf) < 12 {
		return nil, errors.New("short DNS response")
	}
	return buf, nil
}

func dnsExchange(ctx context.Context, addr string, q []byte) ([]byte, error) {
	if len(q) == 0 || len(q) > maxDNSPacket {
		return nil, errors.New("invalid DNS message size")
	}

	// UDP is the common local transport. A truncated response is retried over TCP.
	response, udpErr := dnsExchangeUDP(ctx, addr, q)
	if udpErr == nil && len(response) >= 4 && (response[2]&0x02) != 0 {
		tcpResponse, err := dnsExchangeTCP(ctx, addr, q)
		if err == nil {
			return tcpResponse, nil
		}
	}
	if udpErr != nil {
		return dnsExchangeTCP(ctx, addr, q)
	}
	return response, nil
}

func dohHandler(upstream, path string, maxBody, maxInflight int) http.HandlerFunc {
	inflight := make(chan struct{}, maxInflight)

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}

		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "dnscrypt-proxy DoH gateway\nPOST or GET /dns-query with a DNS message\n")
			return
		}

		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}

		// Public DoH endpoints are commonly used by browser-based clients, so
		// handle CORS consistently for success, errors, and preflight requests.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		var query []byte
		switch r.Method {
		case http.MethodGet:
			encoded := r.URL.Query().Get("dns")
			if encoded == "" {
				http.Error(w, "missing dns query parameter", http.StatusBadRequest)
				return
			}
			var err error
			query, err = decodeQuery(encoded)
			if err != nil {
				http.Error(w, "invalid dns query encoding", http.StatusBadRequest)
				return
			}
			if len(query) == 0 {
				http.Error(w, "empty DNS message", http.StatusBadRequest)
				return
			}
			if len(query) > maxDNSPacket {
				http.Error(w, "DNS message too large", http.StatusRequestEntityTooLarge)
				return
			}

		case http.MethodPost:
			if maxBody <= 0 || maxBody > maxDNSPacket {
				maxBody = defaultMaxBody
			}
			if r.ContentLength > int64(maxBody) {
				http.Error(w, "DNS message too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, int64(maxBody))
			var err error
			query, err = io.ReadAll(r.Body)
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					http.Error(w, "DNS message too large", http.StatusRequestEntityTooLarge)
					return
				}
				http.Error(w, "failed to read dns message", http.StatusBadRequest)
				return
			}
			if len(query) == 0 {
				http.Error(w, "empty DNS message", http.StatusBadRequest)
				return
			}

		default:
			w.Header().Set("Allow", "GET, POST, OPTIONS")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		select {
		case inflight <- struct{}{}:
			defer func() { <-inflight }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "gateway at capacity, retry shortly", http.StatusServiceUnavailable)
			return
		}

		response, err := dnsExchange(r.Context(), upstream, query)
		if err != nil {
			log.Printf("DNS exchange failed: %v", err)
			http.Error(w, "upstream DNS failure", http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/dns-message")
		w.Header().Set("Cache-Control", "no-store")
		// Set explicitly so every response is framed with Content-Length
		// instead of falling back to chunked transfer encoding.
		w.Header().Set("Content-Length", strconv.Itoa(len(response)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(response)
	}
}

func main() {
	port := env("PORT", "8080")
	upstream := env("DOH_UPSTREAM_ADDR", env("DNS_LISTEN", "127.0.0.1:5300"))
	path := env("DOH_PATH", "/dns-query")
	maxBody := envInt("DOH_MAX_BODY", defaultMaxBody)
	if maxBody > maxDNSPacket {
		maxBody = maxDNSPacket
	}
	maxInflight := envInt("DOH_MAX_INFLIGHT", defaultMaxInflight)
	maxConns := envInt("DOH_MAX_CONNS", defaultMaxConns)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	bindHost := env("DOH_BIND", "0.0.0.0")
	addr := net.JoinHostPort(bindHost, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           dohHandler(upstream, path, maxBody, maxInflight),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(fmt.Errorf("DoH gateway: listen: %w", err))
	}
	ln = newLimitListener(ln, maxConns)

	log.Printf("DoH gateway listening on http://%s%s -> %s", addr, path, upstream)
	log.Printf("health endpoint: http://%s/healthz", net.JoinHostPort(bindHost, port))
	log.Printf("limits: max %d in-flight exchanges, max %d connections", maxInflight, maxConns)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(fmt.Errorf("DoH gateway: %w", err))
		}
	case <-sig:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("DoH gateway: shutdown: %v", err)
		}
	}
}
