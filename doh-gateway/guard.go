package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultIPConnLimit          = 64
	defaultDoHIPRequests        = 16
	defaultDoHRequestsPerMinute = 100
	maxDoHRequestsPerMinute     = 100
	defaultDoHClientStates      = 8192
	defaultDoHStateTTL          = 5 * time.Minute
	defaultMaxUDPPacket         = 8 << 10
	defaultMaxTCPFrame          = 8 << 10
	defaultMaxTCPQueries        = 1
	maxIPConnLimit              = 64
	maxDoHIPRequests            = 64
	maxDoHClientStates          = 32768
	minDoHClientStates          = 16
	maxDoHTransportSize         = maxDNSPacket
	clientGuardShardCount       = 16
)

type dnsTransportLimits struct {
	maxUDPPacket  int
	maxTCPFrame   int
	maxTCPQueries int
}

func defaultDNSTransportLimits() dnsTransportLimits {
	return dnsTransportLimits{
		maxUDPPacket:  defaultMaxUDPPacket,
		maxTCPFrame:   defaultMaxTCPFrame,
		maxTCPQueries: defaultMaxTCPQueries,
	}
}

func dnsTransportLimitsFromEnv() dnsTransportLimits {
	defaults := defaultDNSTransportLimits()
	return dnsTransportLimits{
		maxUDPPacket:  envInt("DOH_MAX_UDP_PACKET", defaults.maxUDPPacket, 512, maxDoHTransportSize),
		maxTCPFrame:   envInt("DOH_MAX_TCP_FRAME", defaults.maxTCPFrame, 512, maxDoHTransportSize),
		maxTCPQueries: defaults.maxTCPQueries,
	}
}

type clientGuardConfig struct {
	maxIPConns             int
	maxIPRequests          int
	maxIPRequestsPerMinute int
	maxClientStates        int
	stateTTL               time.Duration
	trustedProxyCIDRs      []netip.Prefix
}

func clientGuardConfigFromEnv() (clientGuardConfig, error) {
	maxIPConns := envInt("IP_CONN_LIMIT", defaultIPConnLimit, 1, maxIPConnLimit)
	maxIPRequests := envInt("DOH_MAX_IP_REQUESTS", defaultDoHIPRequests, 1, maxDoHIPRequests)
	maxIPRequestsPerMinute := envInt("DOH_MAX_IP_REQUESTS_PER_MINUTE", defaultDoHRequestsPerMinute, 1, maxDoHRequestsPerMinute)
	maxStates := envInt("DOH_MAX_CLIENT_STATES", defaultDoHClientStates, minDoHClientStates, maxDoHClientStates)
	maxStates = (maxStates / clientGuardShardCount) * clientGuardShardCount
	if maxStates < minDoHClientStates {
		maxStates = minDoHClientStates
	}

	cidrs, err := parseTrustedProxyCIDRs(os.Getenv("DOH_TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return clientGuardConfig{}, err
	}

	return clientGuardConfig{
		maxIPConns:             maxIPConns,
		maxIPRequests:          maxIPRequests,
		maxIPRequestsPerMinute: maxIPRequestsPerMinute,
		maxClientStates:        maxStates,
		stateTTL:               defaultDoHStateTTL,
		trustedProxyCIDRs:      cidrs,
	}, nil
}

func parseTrustedProxyCIDRs(value string) ([]netip.Prefix, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	parts := strings.Split(value, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, raw := range parts {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid DOH_TRUSTED_PROXY_CIDRS entry %q: %w", raw, err)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

type clientKey struct {
	ip netip.Addr
}

type clientState struct {
	lastSeen       time.Time
	ipConns        int
	requests       int
	rateTimestamps [maxDoHRequestsPerMinute]int64
	rateHead       uint8
	rateCount      uint8
}

type clientShard struct {
	mu    sync.Mutex
	items map[clientKey]*clientState
}

// clientGuard uses fixed-size shards and a hard per-shard entry cap. This keeps
// memory bounded even when an attacker cycles through huge numbers of source IPs.
// Eviction only removes entries with no active connection/request work and no
// active rolling-window quota, so live accounting and rate state are preserved.
type clientGuard struct {
	cfg      clientGuardConfig
	shards   [clientGuardShardCount]clientShard
	shardCap int
}

func newClientGuard(cfg clientGuardConfig) *clientGuard {
	if cfg.maxIPConns <= 0 {
		cfg.maxIPConns = defaultIPConnLimit
	}
	if cfg.maxIPRequests <= 0 {
		cfg.maxIPRequests = defaultDoHIPRequests
	}
	if cfg.maxIPRequestsPerMinute <= 0 {
		cfg.maxIPRequestsPerMinute = defaultDoHRequestsPerMinute
	}
	if cfg.maxIPRequestsPerMinute > maxDoHRequestsPerMinute {
		cfg.maxIPRequestsPerMinute = maxDoHRequestsPerMinute
	}
	if cfg.maxClientStates < minDoHClientStates {
		cfg.maxClientStates = minDoHClientStates
	}
	cfg.maxClientStates = (cfg.maxClientStates / clientGuardShardCount) * clientGuardShardCount
	if cfg.stateTTL <= 0 {
		cfg.stateTTL = defaultDoHStateTTL
	}
	g := &clientGuard{
		cfg:      cfg,
		shardCap: cfg.maxClientStates / clientGuardShardCount,
	}
	for i := range g.shards {
		g.shards[i].items = make(map[clientKey]*clientState, g.shardCap)
	}
	return g
}

func clientShardIndex(key clientKey) int {
	b := key.ip.As16()
	var h uint32 = 2166136261
	for _, x := range b {
		h ^= uint32(x)
		h *= 16777619
	}
	return int(h % clientGuardShardCount)
}

func clientIPKey(ip netip.Addr) clientKey {
	return clientKey{ip: ip}
}

func rateWindowActiveLocked(st *clientState, now time.Time) bool {
	if st.rateCount == 0 {
		return false
	}
	cutoff := now.UnixNano() - int64(time.Minute)
	return st.rateTimestamps[st.rateHead] > cutoff
}

func (g *clientGuard) getStateLocked(s *clientShard, key clientKey, now time.Time) (*clientState, bool) {
	if st, ok := s.items[key]; ok {
		st.lastSeen = now
		return st, true
	}

	if len(s.items) >= g.shardCap {
		var oldestKey clientKey
		var oldest *clientState
		for candidateKey, candidate := range s.items {
			if candidate.ipConns != 0 || candidate.requests != 0 {
				continue
			}
			if rateWindowActiveLocked(candidate, now) {
				// Do not evict a state that still has an active rolling-window
				// quota. Otherwise high-cardinality source-IP churn could reset
				// that client's rate-limit state and bypass the 100/minute cap.
				continue
			}
			if oldest == nil || candidate.lastSeen.Before(oldest.lastSeen) {
				oldestKey = candidateKey
				oldest = candidate
			}
		}
		if oldest != nil {
			delete(s.items, oldestKey)
		} else {
			return nil, false
		}
	}

	st := &clientState{lastSeen: now}
	s.items[key] = st
	return st, true
}

type requestAdmission uint8

const (
	requestAdmissionAllowed requestAdmission = iota
	requestAdmissionConcurrencyLimited
	requestAdmissionRateLimited
)

func (g *clientGuard) admitRequest(ip netip.Addr, now time.Time) requestAdmission {
	key := clientIPKey(ip)
	s := &g.shards[clientShardIndex(key)]
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := g.getStateLocked(s, key, now)
	if !ok {
		return requestAdmissionConcurrencyLimited
	}
	if st.requests >= g.cfg.maxIPRequests {
		// A request rejected only because the concurrency cap is full must not
		// consume a rate token; otherwise a burst of concurrent work could
		// spend the minute quota without doing any DNS exchange.
		return requestAdmissionConcurrencyLimited
	}
	if !g.takeRateTokenLocked(st, now) {
		return requestAdmissionRateLimited
	}
	st.requests++
	st.lastSeen = now
	return requestAdmissionAllowed
}

func (g *clientGuard) beginRequest(ip netip.Addr, now time.Time) bool {
	return g.admitRequest(ip, now) == requestAdmissionAllowed
}

func (g *clientGuard) takeRateTokenLocked(st *clientState, now time.Time) bool {
	const ringSize = maxDoHRequestsPerMinute
	nowNanos := now.UnixNano()
	cutoff := nowNanos - int64(time.Minute)

	for st.rateCount > 0 {
		oldest := st.rateTimestamps[st.rateHead]
		if oldest > cutoff {
			break
		}
		st.rateHead = (st.rateHead + 1) % ringSize
		st.rateCount--
	}

	if int(st.rateCount) >= g.cfg.maxIPRequestsPerMinute {
		st.lastSeen = now
		return false
	}

	index := (int(st.rateHead) + int(st.rateCount)) % ringSize
	st.rateTimestamps[index] = nowNanos
	st.rateCount++
	return true
}

func (g *clientGuard) endRequest(ip netip.Addr, now time.Time) {
	key := clientIPKey(ip)
	s := &g.shards[clientShardIndex(key)]
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.items[key]; ok {
		if st.requests > 0 {
			st.requests--
		}
		st.lastSeen = now
	}
}

func (g *clientGuard) openConnection(ip netip.Addr, now time.Time) bool {
	key := clientIPKey(ip)
	s := &g.shards[clientShardIndex(key)]
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := g.getStateLocked(s, key, now)
	if !ok || st.ipConns >= g.cfg.maxIPConns {
		return false
	}
	st.ipConns++
	st.lastSeen = now
	return true
}

func (g *clientGuard) touchConnection(ip netip.Addr, now time.Time) {
	key := clientIPKey(ip)
	s := &g.shards[clientShardIndex(key)]
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.items[key]; ok {
		st.lastSeen = now
	}
}

func (g *clientGuard) closeConnection(ip netip.Addr, now time.Time) {
	key := clientIPKey(ip)
	s := &g.shards[clientShardIndex(key)]
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.items[key]; ok {
		if st.ipConns > 0 {
			st.ipConns--
		}
		st.lastSeen = now
	}
}

type guardListener struct {
	net.Listener
	guard    *clientGuard
	maxConns int64
	active   atomic.Int64
}

func newGuardListener(l net.Listener, guard *clientGuard, maxConns int) net.Listener {
	if maxConns < 1 {
		maxConns = 1
	}
	return &guardListener{
		Listener: l,
		guard:    guard,
		maxConns: int64(maxConns),
	}
}

func (l *guardListener) reserveGlobal() bool {
	for {
		current := l.active.Load()
		if current >= l.maxConns {
			return false
		}
		if l.active.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (l *guardListener) releaseGlobal() {
	l.active.Add(-1)
}

func (l *guardListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		now := time.Now()
		ip, valid := remoteAddrIP(conn.RemoteAddr())
		if !valid {
			_ = conn.Close()
			continue
		}
		if !l.reserveGlobal() {
			_ = conn.Close()
			continue
		}
		// Direct peers have a stable client identity and can be charged at
		// accept-time. Trusted reverse proxies multiplex users, so their socket
		// address is not charged; the request handler binds the connection to the
		// forwarded client identity before admitting the request.
		tracked := !trustedProxyContains(l.guard.cfg.trustedProxyCIDRs, ip)
		if tracked && !l.guard.openConnection(ip, now) {
			l.releaseGlobal()
			_ = conn.Close()
			continue
		}

		return &guardConn{
			Conn:          conn,
			guard:         l.guard,
			ip:            ip,
			tracked:       tracked,
			globalRelease: l.releaseGlobal,
		}, nil
	}
}

func (l *guardListener) Close() error {
	return l.Listener.Close()
}

type guardConn struct {
	net.Conn
	guard         *clientGuard
	stateMu       sync.Mutex
	ip            netip.Addr
	tracked       bool // counted against the per-source-IP connection cap
	closed        bool
	globalRelease func()
	once          sync.Once
}

func (c *guardConn) rebindClient(ip netip.Addr, now time.Time) bool {
	if !ip.IsValid() || ip.IsUnspecified() {
		return false
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.closed {
		return false
	}
	if c.tracked && c.ip == ip {
		c.guard.touchConnection(ip, now)
		return true
	}
	if !c.guard.openConnection(ip, now) {
		return false
	}
	if c.tracked {
		c.guard.closeConnection(c.ip, now)
	}
	c.ip = ip
	c.tracked = true
	return true
}

func (c *guardConn) Close() error {
	var err error
	c.once.Do(func() {
		c.stateMu.Lock()
		c.closed = true
		ip := c.ip
		tracked := c.tracked
		c.stateMu.Unlock()

		err = c.Conn.Close()
		if tracked {
			c.guard.closeConnection(ip, time.Now())
		}
		c.globalRelease()
	})
	return err
}

func remoteAddrIP(addr net.Addr) (netip.Addr, bool) {
	if addr == nil {
		return netip.Addr{}, false
	}
	return remoteAddrIPString(addr.String())
}

func trustedProxyContains(prefixes []netip.Prefix, ip netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// requestClientIP uses the direct peer when no trusted reverse proxy is
// configured. For a trusted proxy, only the final X-Forwarded-For value is used;
// a missing or malformed forwarded identity is rejected rather than pooled into
// a shared proxy bucket.
func requestClientIP(r *http.Request, trustedProxies []netip.Prefix) (netip.Addr, bool) {
	remote, ok := remoteAddrIPString(r.RemoteAddr)
	if !ok || remote.IsUnspecified() {
		return netip.Addr{}, false
	}
	if len(trustedProxies) == 0 || !trustedProxyContains(trustedProxies, remote) {
		return remote, true
	}

	values := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(values) == 0 {
		return netip.Addr{}, false
	}
	final := strings.TrimSpace(values[len(values)-1])
	if final == "" {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(final)
	if err != nil || !ip.IsValid() || ip.IsUnspecified() {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func remoteAddrIPString(addr string) (netip.Addr, bool) {
	text := strings.TrimSpace(addr)
	if host, _, err := net.SplitHostPort(text); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil && ip.IsValid() {
			return ip.Unmap(), true
		}
	}
	if ip, err := netip.ParseAddr(text); err == nil && ip.IsValid() {
		return ip.Unmap(), true
	}
	return netip.Addr{}, false
}

var errTCPQueriesExceeded = errors.New("maximum TCP DNS queries per connection exceeded")

// guardedTCPDNSConn is deliberately one-shot in the current DoH path, but it
// retains an explicit query counter so a future TCP keep-alive path cannot
// accidentally turn into an unbounded DNS connection.
type guardedTCPDNSConn struct {
	conn       net.Conn
	maxFrame   int
	maxQueries int
	queries    int
}

func (c *guardedTCPDNSConn) exchange(ctx context.Context, q []byte) ([]byte, error) {
	if c.queries >= c.maxQueries {
		return nil, errTCPQueriesExceeded
	}
	if len(q) == 0 || len(q) > c.maxFrame {
		return nil, errors.New("DNS query exceeds TCP frame limit")
	}
	c.queries++

	setConnDeadline(ctx, c.conn, defaultServerTimeout)
	frame := []byte{byte(len(q) >> 8), byte(len(q))}
	frame = append(frame, q...)
	if _, err := c.conn.Write(frame); err != nil {
		return nil, err
	}

	var hdr [2]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return nil, err
	}
	n := int(hdr[0])<<8 | int(hdr[1])
	if n <= 0 || n > c.maxFrame {
		return nil, errors.New("invalid TCP DNS response size")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		return nil, err
	}
	if len(buf) < 12 {
		return nil, errors.New("short DNS response")
	}
	return buf, nil
}
