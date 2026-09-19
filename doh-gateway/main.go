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
	maxDNSPacket = 65535

	// dnsExchangeTimeout bounds one full DNS exchange: the UDP attempt plus,
	// if the response comes back truncated, the TCP retry.
	dnsExchangeTimeout = 5 * time.Second

	// httpWriteTimeout must stay comfortably above dnsExchangeTimeout.
	// net/http's WriteTimeout deadline is set once, when request headers are
	// read, and covers the entire handler plus the response write.
	httpWriteTimeout = 7 * time.Second
	httpReadTimeout  = 5 * time.Second

	defaultMaxBody     = 8 << 10 // 8 KiB request query limit
	defaultMaxInflight = 32
	defaultMaxConns    = 128
	maxMaxInflight     = 64
	maxMaxConns        = 256
	maxHeaderBytes     = 8 << 10 // 8 KiB

	readyProbeTimeout = 500 * time.Millisecond
)

// Pre-computed health endpoint bodies: avoids a string-to-[]byte conversion
// on every health check request.
var (
	healthzBody           = []byte("ok\n")
	readyzBody            = []byte("ready\n")
	readyzUnavailableBody = []byte("upstream unavailable\n")
	indexBody             = []byte("dnscrypt-proxy DoH gateway\nPOST or GET /dns-query with a DNS message\n")
)

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback, min, max int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return fallback
	}
	if n > max {
		return max
	}
	return n
}

// limitListener bounds simultaneously open TCP connections without making
// shutdown dependent on an Accept call that is blocked waiting for capacity.
type limitListener struct {
	net.Listener
	sem      chan struct{}
	done     chan struct{}
	closeOne sync.Once
}

func newLimitListener(l net.Listener, maxConns int) net.Listener {
	if maxConns < 1 {
		maxConns = 1
	}
	return &limitListener{
		Listener: l,
		sem:      make(chan struct{}, maxConns),
		done:     make(chan struct{}),
	}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}

	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitListenerConn{Conn: c, release: l.sem}, nil
}

func (l *limitListener) Close() error {
	l.closeOne.Do(func() { close(l.done) })
	return l.Listener.Close()
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

func validateDNSQuery(query []byte) error {
	if len(query) < 12 {
		return errors.New("short DNS query")
	}
	// QR=1 means the message is a response. A public DoH gateway should only
	// forward query messages to its recursive resolver.
	if query[2]&0x80 != 0 {
		return errors.New("DNS response supplied as query")
	}
	return nil
}

// watchCancel closes the local socket as soon as the HTTP request context is
// canceled. context.AfterFunc avoids a permanently waiting goroutine per
// exchange while still interrupting blocked network I/O promptly.
func watchCancel(ctx context.Context, conn net.Conn) (stop func()) {
	cancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return func() { _ = cancel() }
}

func setConnDeadline(ctx context.Context, conn net.Conn) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(dnsExchangeTimeout)
	}
	_ = conn.SetDeadline(deadline)
}

// udpBufPool recycles the receive buffer for UDP responses. The buffer must be
// the full 64 KiB: a UDP read into a smaller slice silently drops the excess
// without setting the TC bit, which would corrupt large EDNS responses. Without
// the pool every request allocates 64 KiB, which is heavy GC churn under the
// gateway's small (24 MiB) heap target.
var udpBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, maxDNSPacket)
		return &b
	},
}

func dnsExchangeUDP(ctx context.Context, addr string, q []byte) ([]byte, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer watchCancel(ctx, conn)()
	setConnDeadline(ctx, conn)

	if _, err := conn.Write(q); err != nil {
		return nil, err
	}

	bufp := udpBufPool.Get().(*[]byte)
	defer udpBufPool.Put(bufp)

	n, err := conn.Read(*bufp)
	if err != nil {
		return nil, err
	}
	if n < 12 {
		return nil, errors.New("short DNS response")
	}
	// Copy out: the pooled buffer is reused as soon as this function returns.
	return append([]byte(nil), (*bufp)[:n]...), nil
}

func dnsExchangeTCP(ctx context.Context, addr string, q []byte) ([]byte, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer watchCancel(ctx, conn)()
	setConnDeadline(ctx, conn)

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
	// Note: DNS query validation (minimum length, QR bit) is performed by the
	// HTTP handler before this function is called; re-validating here would be
	// redundant work on the hot path.

	// Keep UDP and TCP fallback inside one total budget so a failed UDP
	// exchange cannot create a second full timeout window.
	ctx, cancel := context.WithTimeout(ctx, dnsExchangeTimeout)
	defer cancel()

	// UDP is the common local transport. A truncated response is retried over TCP.
	response, udpErr := dnsExchangeUDP(ctx, addr, q)
	if udpErr == nil {
		if len(response) >= 4 && (response[2]&0x02) != 0 {
			tcpResponse, tcpErr := dnsExchangeTCP(ctx, addr, q)
			if tcpErr != nil {
				return nil, fmt.Errorf("TCP fallback: %w", tcpErr)
			}
			return tcpResponse, nil
		}
		return response, nil
	}

	// If the UDP attempt already spent the shared budget (timeout, or the
	// client went away), a TCP retry cannot succeed. Return the real UDP error
	// rather than a misleading TCP dial failure.
	var netErr net.Error
	if ctx.Err() != nil || (errors.As(udpErr, &netErr) && netErr.Timeout()) {
		return nil, udpErr
	}

	return dnsExchangeTCP(ctx, addr, q)
}

func upstreamReady(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, readyProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func dohHandler(upstream, path string, maxBody, maxInflight int) http.HandlerFunc {
	if maxBody < 12 || maxBody > maxDNSPacket {
		maxBody = defaultMaxBody
	}
	if maxInflight < 1 || maxInflight > maxMaxInflight {
		maxInflight = defaultMaxInflight
	}
	inflight := make(chan struct{}, maxInflight)

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(healthzBody)
			return
		}

		if r.URL.Path == "/readyz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if upstreamReady(upstream) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(readyzBody)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(readyzUnavailableBody)
			return
		}

		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(indexBody)
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
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		var query []byte
		switch r.Method {
		case http.MethodGet:
			encoded := r.URL.Query().Get("dns")
			if encoded == "" {
				writeText(w, http.StatusBadRequest, "missing dns query parameter\n")
				return
			}
			var err error
			query, err = decodeQuery(encoded)
			if err != nil {
				writeText(w, http.StatusBadRequest, "invalid dns query encoding\n")
				return
			}
			if len(query) > maxBody {
				writeText(w, http.StatusRequestEntityTooLarge, "DNS message too large\n")
				return
			}
			if err := validateDNSQuery(query); err != nil {
				writeText(w, http.StatusBadRequest, "invalid DNS query\n")
				return
			}

		case http.MethodPost:
			if r.ContentLength > int64(maxBody) {
				writeText(w, http.StatusRequestEntityTooLarge, "DNS message too large\n")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, int64(maxBody))
			var err error
			query, err = io.ReadAll(r.Body)
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					writeText(w, http.StatusRequestEntityTooLarge, "DNS message too large\n")
					return
				}
				writeText(w, http.StatusBadRequest, "failed to read DNS message\n")
				return
			}
			if err := validateDNSQuery(query); err != nil {
				writeText(w, http.StatusBadRequest, "invalid DNS query\n")
				return
			}

		default:
			w.Header().Set("Allow", "GET, POST, OPTIONS")
			writeText(w, http.StatusMethodNotAllowed, "method not allowed\n")
			return
		}

		select {
		case inflight <- struct{}{}:
			defer func() { <-inflight }()
		default:
			w.Header().Set("Retry-After", "1")
			writeText(w, http.StatusServiceUnavailable, "gateway at capacity, retry shortly\n")
			return
		}

		response, err := dnsExchange(r.Context(), upstream, query)
		if err != nil {
			if r.Context().Err() != nil {
				// The client disconnected; there is nobody to answer and
				// nothing worth logging.
				return
			}
			log.Printf("DNS exchange failed: %v", err)
			writeText(w, http.StatusBadGateway, "upstream DNS failure\n")
			return
		}

		w.Header().Set("Content-Type", "application/dns-message")
		w.Header().Set("Content-Length", strconv.Itoa(len(response)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(response)
	}
}

func main() {
	port := env("PORT", "8080")
	upstream := env("DOH_UPSTREAM_ADDR", "127.0.0.1:5300")
	path := env("DOH_PATH", "/dns-query")
	maxBody := envInt("DOH_MAX_BODY", defaultMaxBody, 12, maxDNSPacket)
	maxInflight := envInt("DOH_MAX_INFLIGHT", defaultMaxInflight, 1, maxMaxInflight)
	maxConns := envInt("DOH_MAX_CONNS", defaultMaxConns, 1, maxMaxConns)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if path == "/" || path == "/healthz" || path == "/readyz" {
		path = "/dns-query"
	}

	bindHost := env("DOH_BIND", "0.0.0.0")
	addr := net.JoinHostPort(bindHost, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           dohHandler(upstream, path, maxBody, maxInflight),
		ReadHeaderTimeout: httpReadTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(fmt.Errorf("DoH gateway: listen: %w", err))
	}
	ln = newLimitListener(ln, maxConns)

	log.Printf("DoH gateway listening on http://%s%s -> %s", addr, path, upstream)
	log.Printf("health endpoints: http://%s/healthz and /readyz", net.JoinHostPort(bindHost, port))
	log.Printf("limits: max %d in-flight exchanges, max %d connections, max %d-byte query", maxInflight, maxConns, maxBody)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(fmt.Errorf("DoH gateway: %w", err))
		}
	case <-sig:
		ctx, cancel := context.WithTimeout(context.Background(), httpWriteTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("DoH gateway: shutdown: %v", err)
		}
	}
}
