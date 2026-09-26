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
	maxDNSPacket     = 65535
	fixedDNSUpstream = "127.0.0.1:5300"

	// defaultServerTimeout bounds one full local DNS exchange: the UDP attempt
	// plus, if the response is truncated or UDP fails early, the TCP retry.
	defaultServerTimeout = 6 * time.Second

	// httpWriteTimeout must stay comfortably above defaultServerTimeout.
	// net/http's WriteTimeout deadline is set once, when request headers are
	// read, and covers the entire handler plus the response write.
	httpWriteTimeout = 8 * time.Second
	httpReadTimeout  = 5 * time.Second

	defaultMaxBody        = 4 << 10 // 4 KiB request query limit
	defaultMaxInflight    = 64
	defaultMaxConns       = 512
	maxMaxInflight        = 64
	maxMaxConns           = 512
	defaultDoHIdleTimeout = 120 * time.Second

	// headerOverhead budgets for the request line/method/host and the fixed
	// set of headers the handler reads (Content-Type, Accept, etc.), on top
	// of whatever the query itself needs.
	headerOverhead = 2 << 10 // 2 KiB

	readyProbeTimeout = 500 * time.Millisecond

	// readyCacheTTL coalesces /readyz probes. /readyz is reachable without the
	// per-client guard, so without a short cache every request would open a
	// fresh TCP connection to the local resolver.
	readyCacheTTL = time.Second
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

// envInt parses an unsigned decimal: surrounding whitespace is ignored, the
// rest must be non-empty ASCII digits (no sign). Values below min fall back to
// the default, while values above max (including integer overflow) clamp to
// max.
func envInt(key string, fallback, min, max int) int {
	v := strings.TrimSpace(os.Getenv(key))
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

func serverTimeoutFromEnv() time.Duration {
	seconds := envInt("SERVER_TIMEOUT", int(defaultServerTimeout/time.Second), 1, 60)
	return time.Duration(seconds) * time.Second
}

func bindHostFromEnv() (string, error) {
	host := env("DOH_BIND", "0.0.0.0")
	if host == "0.0.0.0" || host == "::" {
		return host, nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("DOH_BIND must be an IP address, got %q", host)
	}
	return ip.String(), nil
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

func setConnDeadline(ctx context.Context, conn net.Conn, fallbackTimeout time.Duration) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(fallbackTimeout)
	}
	_ = conn.SetDeadline(deadline)
}

// udpBufPool recycles the receive buffer for UDP responses. It stays at the DNS
// wire maximum so the guard can distinguish normal packets from oversized
// datagrams without allocating a second buffer on the hot path. Without the
// pool every request allocates 64 KiB, which is avoidable GC churn under the
// gateway's small 80 MiB heap target.
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
	setConnDeadline(ctx, conn, defaultServerTimeout)
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
		response := (*bufp)[:n]
		// Validate before returning a backend response to the HTTP layer. A local
		// resolver that returns malformed or unrelated data must become 502, never
		// a successful application/dns-message response.
		if err := validateDNSResponse(q, response); err != nil {
			return nil, err
		}
		// Copy out: the pooled buffer is reused as soon as this function returns.
		return append([]byte(nil), response...), nil
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
	response, err := tcpConn.exchange(ctx, q)
	if err != nil {
		return nil, err
	}
	if err := validateDNSResponse(q, response); err != nil {
		return nil, err
	}
	return response, nil
}

func dnsExchangeWithLimits(ctx context.Context, addr string, q []byte, limits dnsTransportLimits) ([]byte, error) {
	return dnsExchangeWithTimeout(ctx, addr, q, limits, defaultServerTimeout)
}

func dnsExchangeWithTimeout(ctx context.Context, addr string, q []byte, limits dnsTransportLimits, timeout time.Duration) ([]byte, error) {
	if len(q) == 0 || len(q) > maxDNSPacket || len(q) > limits.maxTCPFrame {
		return nil, errors.New("invalid DNS message size")
	}
	if timeout <= 0 {
		timeout = defaultServerTimeout
	}

	// Keep UDP and TCP fallback inside one total budget so a failed UDP
	// exchange cannot create a second full timeout window.
	ctx, cancel := context.WithTimeout(ctx, timeout)
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

func validateDNSResponse(query, response []byte) error {
	if len(response) < 12 {
		return errors.New("short DNS response")
	}
	if len(query) < 12 {
		return errors.New("short DNS query")
	}
	if response[0] != query[0] || response[1] != query[1] {
		return errors.New("DNS response transaction ID mismatch")
	}
	if response[2]&0x80 == 0 {
		return errors.New("DNS response missing QR bit")
	}
	if response[2]&0x78 != query[2]&0x78 {
		return errors.New("DNS response opcode mismatch")
	}
	return nil
}

func upstreamReady(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, readyProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// readyProbe caches the result of upstreamReady for readyCacheTTL. The mutex
// is held across the probe so concurrent /readyz requests share one dial.
type readyProbe struct {
	mu sync.Mutex
	at time.Time
	ok bool
}

func (p *readyProbe) ready(addr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && time.Since(p.at) < readyCacheTTL {
		return p.ok
	}
	p.ok = upstreamReady(addr)
	p.at = time.Now()
	return p.ok
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

func dohHandlerWithGuardTimeout(upstream, path string, maxBody, maxInflight int, transport dnsTransportLimits, guard *clientGuard, trustedProxies []netip.Prefix, serverTimeout time.Duration) http.HandlerFunc {
	maxBody, maxInflight, transport = effectiveDoHLimits(maxBody, maxInflight, transport)
	inflight := make(chan struct{}, maxInflight)
	var probe readyProbe

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
			if probe.ready(upstream) {
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

		if guard != nil {
			clientIP, valid := requestClientIP(r, trustedProxies)
			if !valid {
				writeText(w, http.StatusBadRequest, "invalid or missing client identity\n")
				return
			}
			now := time.Now()
			if conn := guardConnFromContext(r.Context()); conn != nil && !conn.rebindClient(clientIP, now) {
				w.Header().Set("Retry-After", "1")
				writeText(w, http.StatusServiceUnavailable, "client connection limit exceeded\n")
				return
			}
			switch guard.admitRequest(clientIP, now) {
			case requestAdmissionRateLimited:
				w.Header().Set("Retry-After", "1")
				writeText(w, http.StatusTooManyRequests, "source IP request rate limit exceeded\n")
				return
			case requestAdmissionConcurrencyLimited:
				w.Header().Set("Retry-After", "1")
				writeText(w, http.StatusServiceUnavailable, "client concurrency limit exceeded\n")
				return
			}
			// Evaluate time.Now() when the request finishes, not when the
			// defer statement is reached.
			defer func() { guard.endRequest(clientIP, time.Now()) }()
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

		response, err := dnsExchangeWithTimeout(r.Context(), upstream, query, transport, serverTimeout)
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

func dohHandlerWithGuard(upstream, path string, maxBody, maxInflight int, transport dnsTransportLimits, guard *clientGuard, trustedProxies []netip.Prefix) http.HandlerFunc {
	return dohHandlerWithGuardTimeout(upstream, path, maxBody, maxInflight, transport, guard, trustedProxies, defaultServerTimeout)
}

type guardConnContextKey struct{}

func guardConnFromContext(ctx context.Context) *guardConn {
	if conn, ok := ctx.Value(guardConnContextKey{}).(*guardConn); ok {
		return conn
	}
	return nil
}

func globalConnLimitFromEnv() int {
	legacy := envInt("DOH_MAX_CONNS", defaultMaxConns, 1, maxMaxConns)
	return envInt("GLOBAL_CONN_LIMIT", legacy, 1, maxMaxConns)
}

func main() {
	port := env("PORT", "8080")
	upstream := fixedDNSUpstream
	path := env("DOH_PATH", "/dns-query")
	maxBody := envInt("DOH_MAX_BODY", defaultMaxBody, 12, maxDNSPacket)
	maxInflight := envInt("DOH_MAX_INFLIGHT", defaultMaxInflight, 1, maxMaxInflight)
	maxConns := globalConnLimitFromEnv()
	dohIdleTimeoutSeconds := envInt("DOH_IDLE_TIMEOUT", int(defaultDoHIdleTimeout/time.Second), 1, 3600)
	dohIdleTimeout := time.Duration(dohIdleTimeoutSeconds) * time.Second
	serverTimeout := serverTimeoutFromEnv()
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
		log.Printf("DOH_PATH %q is reserved; using /dns-query", path)
		path = "/dns-query"
	}

	// Normalize once, up front, so MaxHeaderBytes below is sized from exactly
	// the same clamped maxBody that dohHandlerWithGuard will enforce, rather
	// than from the raw pre-clamp DOH_MAX_BODY value.
	effMaxBody, effMaxInflight, effTransport := effectiveDoHLimits(maxBody, maxInflight, transport)

	bindHost, err := bindHostFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	addr := net.JoinHostPort(bindHost, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           dohHandlerWithGuardTimeout(upstream, path, effMaxBody, effMaxInflight, effTransport, guard, guardConfig.trustedProxyCIDRs, serverTimeout),
		ReadHeaderTimeout: httpReadTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       dohIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytesFor(effMaxBody),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if gc, ok := c.(*guardConn); ok {
				return context.WithValue(ctx, guardConnContextKey{}, gc)
			}
			return ctx
		},
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(fmt.Errorf("DoH gateway: listen: %w", err))
	}
	ln = newGuardListener(ln, guard, maxConns)

	log.Printf("DoH gateway listening on http://%s%s -> %s", addr, path, upstream)
	log.Printf("health endpoints: http://%s/healthz and /readyz", addr)
	log.Printf("limits: max %d in-flight exchanges, max %d TCP connections, max %d-byte DoH query", effMaxInflight, maxConns, effMaxBody)
	log.Printf("resource guard: max %d conns per source IP, %d concurrent requests per source IP, max %d client states", guardConfig.maxIPConns, guardConfig.maxIPRequests, guardConfig.maxClientStates)
	if len(guardConfig.trustedProxyCIDRs) == 0 {
		log.Printf("DOH_TRUSTED_PROXY_CIDRS is unset: clients are identified by the TCP peer address. Behind a reverse proxy every user then shares the same source-IP concurrency bucket; set it to the proxy's CIDRs")
	}
	log.Printf("DNS transport guard: UDP %d bytes, TCP frame %d bytes, max %d TCP queries/connection, SERVER_TIMEOUT=%s", effTransport.maxUDPPacket, effTransport.maxTCPFrame, effTransport.maxTCPQueries, serverTimeout)

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
