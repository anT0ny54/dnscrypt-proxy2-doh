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
	"net/netip"
	"net/url"
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

	// headerOverhead budgets for the request line/method/host and the fixed
	// set of headers the handler reads (Content-Type, Accept, etc.), on top
	// of whatever the query itself needs.
	headerOverhead = 2 << 10 // 2 KiB

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

// maxHeaderBytesFor sizes net/http's MaxHeaderBytes for the worst supported
// GET representation. Base64 expands the DNS message by 4/3, and a client can
// percent-escape every Base64 character, expanding those bytes by another 3x.
// This keeps an oversized GET inside header parsing far enough to reach the
// handler's client-friendly 413 response instead of being rejected as 431.
func maxHeaderBytesFor(maxBody int) int {
	return maxBody*4 + headerOverhead
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// envInt parses the same unsigned-decimal syntax used by start.sh:
// non-empty ASCII digits only, no trimming or leading '+'. Values below min
// fall back to the default, while values above max (including integer
// overflow) clamp to max.
func envInt(key string, fallback, min, max int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}

	n := 0
	overflow := false
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return fallback
		}
		digit := int(v[i] - '0')
		if !overflow {
			if n > (max-digit)/10 {
				overflow = true
			} else {
				n = n*10 + digit
			}
		}
	}

	if overflow || n > max {
		return max
	}
	if n < min {
		return fallback
	}
	return n
}

func envFloat(key string, fallback, min, max float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < min {
		return fallback
	}
	if n > max {
		return max
	}
	return n
}

// decodeQuery decodes a DoH "dns" GET parameter, which in practice arrives in
// one of four base64 flavors: URL-safe or standard alphabet, each padded or
// unpadded. The alphabet and padding are cheap to detect up front, so rather
// than trying all four encodings on every request, only the one or two that
// could possibly match are attempted.
func decodeQuery(v string) ([]byte, error) {
	urlSafe := false
	std := false
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '-', '_':
			urlSafe = true
		case '+', '/':
			std = true
		}
	}

	padded := len(v) > 0 && v[len(v)-1] == '='

	var encodings []*base64.Encoding
	switch {
	case urlSafe:
		// '-'/'_' only appear in the URL-safe alphabet.
		if padded {
			encodings = []*base64.Encoding{base64.URLEncoding}
		} else {
			encodings = []*base64.Encoding{base64.RawURLEncoding}
		}
	case std:
		// '+'/'/' only appear in the standard alphabet.
		if padded {
			encodings = []*base64.Encoding{base64.StdEncoding}
		} else {
			encodings = []*base64.Encoding{base64.RawStdEncoding}
		}
	case padded:
		// Padded but no alphabet-distinguishing character seen: the URL-safe
		// and standard alphabets agree on every other character.
		encodings = []*base64.Encoding{base64.URLEncoding, base64.StdEncoding}
	default:
		// No padding and no distinguishing character: try both raw variants.
		encodings = []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding}
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

// rawQueryParam extracts one query parameter using URI percent-decoding, not
// form decoding. This matters for legacy standard Base64 DoH clients: '+' is a
// valid Base64 character, while net/url.Values treats an unescaped '+' as a
// space. URL-escaped '+' (%2B) therefore remains a literal '+'.
func rawQueryParam(rawQuery, wantKey string) (string, error) {
	for _, field := range strings.Split(rawQuery, "&") {
		if field == "" {
			continue
		}
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		key, err := url.PathUnescape(key)
		if err != nil {
			continue
		}
		if key != wantKey {
			continue
		}
		value, err = url.PathUnescape(value)
		if err != nil {
			return "", err
		}
		return value, nil
	}
	return "", nil
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

// udpBufPool recycles the receive buffer for UDP responses. It stays at the DNS
// wire maximum so the guard can distinguish normal packets from oversized
// datagrams without allocating a second buffer on the hot path. Without the
// pool every request allocates 64 KiB, which is heavy GC churn under the
// gateway's small (24 MiB) heap target.
var udpBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, maxDNSPacket)
		return &b
	},
}

func dnsExchangeUDP(ctx context.Context, addr string, q []byte, maxUDPPacket int) ([]byte, error) {
	if len(q) == 0 || len(q) > maxUDPPacket {
		return nil, errors.New("DNS query exceeds UDP packet limit")
	}

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

	for {
		n, err := conn.Read(*bufp)
		if err != nil {
			return nil, err
		}
		if n > maxUDPPacket {
			// UDP overflow is an abuse/garbage case at this layer. The datagram
			// has already been fully discarded by the kernel; keep waiting for a
			// bounded time instead of turning it into a second allocation/parse.
			continue
		}
		if n < 12 {
			return nil, errors.New("short DNS response")
		}
		// Copy out: the pooled buffer is reused as soon as this function returns.
		return append([]byte(nil), (*bufp)[:n]...), nil
	}
}

func dnsExchangeTCP(ctx context.Context, addr string, q []byte, limits dnsTransportLimits) ([]byte, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer watchCancel(ctx, conn)()

	tcpConn := guardedTCPDNSConn{
		conn:       conn,
		maxFrame:   limits.maxTCPFrame,
		maxQueries: limits.maxTCPQueries,
	}
	return tcpConn.exchange(ctx, q)
}

func dnsExchangeWithLimits(ctx context.Context, addr string, q []byte, limits dnsTransportLimits) ([]byte, error) {
	if len(q) == 0 || len(q) > maxDNSPacket || len(q) > limits.maxTCPFrame {
		return nil, errors.New("invalid DNS message size")
	}
	// Note: DNS query validation (minimum length, QR bit) is performed by the
	// HTTP handler before this function is called; re-validating here would be
	// redundant work on the hot path.

	// Keep UDP and TCP fallback inside one total budget so a failed UDP
	// exchange cannot create a second full timeout window.
	ctx, cancel := context.WithTimeout(ctx, dnsExchangeTimeout)
	defer cancel()

	// Messages larger than the configured UDP packet budget go straight to TCP.
	if len(q) > limits.maxUDPPacket {
		return dnsExchangeTCP(ctx, addr, q, limits)
	}

	// UDP is the common local transport. A truncated response is retried over TCP.
	response, udpErr := dnsExchangeUDP(ctx, addr, q, limits.maxUDPPacket)
	if udpErr == nil {
		if len(response) >= 4 && (response[2]&0x02) != 0 {
			tcpResponse, tcpErr := dnsExchangeTCP(ctx, addr, q, limits)
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

	return dnsExchangeTCP(ctx, addr, q, limits)
}

func upstreamReady(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, readyProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// writeText writes a plain-text status response. Content-Type is set here,
// before WriteHeader, because every call site is an error/status path with
// no other opportunity to set it: once WriteHeader runs, net/http's
// automatic Content-Type sniffing (which only applies if no Content-Type
// was set before the first Write) no longer has a chance to add one.
func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// effectiveDoHLimits normalizes the handler's configuration knobs the same
// way dohHandlerWithGuard does internally, so that anything sizing other
// parts of the server (e.g. net/http's MaxHeaderBytes) can be computed from
// the same clamped values the handler actually enforces, rather than from
// the raw, pre-clamp input. In particular maxBody can never exceed
// transport.maxTCPFrame once this returns.
func effectiveDoHLimits(maxBody, maxInflight int, transport dnsTransportLimits) (effMaxBody, effMaxInflight int, effTransport dnsTransportLimits) {
	if maxBody < 12 || maxBody > maxDNSPacket {
		maxBody = defaultMaxBody
	}
	if maxInflight < 1 || maxInflight > maxMaxInflight {
		maxInflight = defaultMaxInflight
	}
	// Keep HTTP request size and the TCP frame guard aligned. UDP-sized queries
	// can use UDP directly; larger queries are sent to the one-shot TCP path.
	if transport.maxTCPFrame < 12 {
		transport.maxTCPFrame = defaultMaxTCPFrame
	}
	if maxBody > transport.maxTCPFrame {
		maxBody = transport.maxTCPFrame
	}
	return maxBody, maxInflight, transport
}

func dohHandlerWithGuard(upstream, path string, maxBody, maxInflight int, transport dnsTransportLimits, guard *clientGuard, trustedProxies []netip.Prefix) http.HandlerFunc {
	maxBody, maxInflight, transport = effectiveDoHLimits(maxBody, maxInflight, transport)
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

		var sourceIP netip.Addr
		if guard != nil {
			sourceIP = requestClientIP(r, trustedProxies)
			if !guard.beginRequest(sourceIP, time.Now()) {
				w.Header().Set("Retry-After", "1")
				writeText(w, http.StatusTooManyRequests, "client rate/concurrency limit exceeded\n")
				return
			}
			defer guard.endRequest(sourceIP, time.Now())
		}

		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST, OPTIONS")
			writeText(w, http.StatusMethodNotAllowed, "method not allowed\n")
			return
		}

		// Acquire the in-flight slot before doing any work reading or decoding
		// the query. At capacity this lets the gateway fail fast with a cheap
		// 503 instead of first reading a POST body (which, for a body close to
		// maxBody, is real allocation and I/O) only to discard it immediately
		// afterward.
		select {
		case inflight <- struct{}{}:
			defer func() { <-inflight }()
		default:
			w.Header().Set("Retry-After", "1")
			writeText(w, http.StatusServiceUnavailable, "gateway at capacity, retry shortly\n")
			return
		}

		var query []byte
		switch r.Method {
		case http.MethodGet:
			encoded, err := rawQueryParam(r.URL.RawQuery, "dns")
			if err != nil {
				writeText(w, http.StatusBadRequest, "invalid dns query parameter\n")
				return
			}
			if encoded == "" {
				writeText(w, http.StatusBadRequest, "missing dns query parameter\n")
				return
			}
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
		}

		response, err := dnsExchangeWithLimits(r.Context(), upstream, query, transport)
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
	dohIdleTimeout := time.Duration(envInt("DOH_IDLE_TIMEOUT", 120, 1, 3600)) * time.Second
	transport := dnsTransportLimitsFromEnv()
	guardConfig, err := clientGuardConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	guard := newClientGuard(guardConfig)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if path == "/" || path == "/healthz" || path == "/readyz" {
		path = "/dns-query"
	}

	// Normalize once, up front, so MaxHeaderBytes below is sized from exactly
	// the same clamped maxBody that dohHandlerWithGuard will enforce, rather
	// than from the raw pre-clamp DOH_MAX_BODY value.
	effMaxBody, effMaxInflight, effTransport := effectiveDoHLimits(maxBody, maxInflight, transport)

	bindHost := env("DOH_BIND", "0.0.0.0")
	addr := net.JoinHostPort(bindHost, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           dohHandlerWithGuard(upstream, path, effMaxBody, effMaxInflight, effTransport, guard, guardConfig.trustedProxyCIDRs),
		ReadHeaderTimeout: httpReadTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       dohIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytesFor(effMaxBody),
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(fmt.Errorf("DoH gateway: listen: %w", err))
	}
	ln = newGuardListener(ln, guard, maxConns)

	log.Printf("DoH gateway listening on http://%s%s -> %s", addr, path, upstream)
	log.Printf("health endpoints: http://%s/healthz and /readyz", net.JoinHostPort(bindHost, port))
	log.Printf("limits: max %d in-flight exchanges, max %d TCP connections, max %d-byte DoH query", effMaxInflight, maxConns, effMaxBody)
	log.Printf("abuse guard: %.1f rps / burst %d, max %d conns + %d requests per source IP, max %d client states", guardConfig.rps, guardConfig.burst, guardConfig.maxIPConns, guardConfig.maxIPRequests, guardConfig.maxClientStates)
	log.Printf("DNS transport guard: UDP %d bytes, TCP frame %d bytes, max %d TCP queries/connection", effTransport.maxUDPPacket, effTransport.maxTCPFrame, effTransport.maxTCPQueries)

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
