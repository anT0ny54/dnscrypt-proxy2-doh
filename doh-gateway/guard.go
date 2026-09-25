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
	defaultDoHRateLimit    = 12.0 // sustained requests/sec per source IP
	defaultDoHRateBurst    = 200
	defaultGlobalRateLimit = 80.0 // sustained requests/sec across the gateway
	defaultGlobalRateBurst = 200
	defaultIPConnLimit     = 32
	defaultDoHIPRequests   = 64
	defaultDoHClientStates = 8192
	defaultDoHStateTTL     = 5 * time.Minute
	defaultMaxUDPPacket    = 8 << 10
	defaultMaxTCPFrame     = 8 << 10
	defaultMaxTCPQueries   = 1
	maxDoHRateLimit        = 100.0
	maxDoHRateBurst        = 256
	maxGlobalRateLimit     = 500.0
	maxGlobalRateBurst     = 512
	maxIPConnLimit         = 32
	maxDoHIPRequests       = 64
	maxDoHClientStates     = 32768
	minDoHClientStates     = 16
	maxDoHTransportSize    = maxDNSPacket
	clientGuardShardCount  = 16
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
	rateLimit         float64
	rateBurst         int
	globalRateLimit   float64
	globalRateBurst   int
	maxIPConns        int
	maxIPRequests     int
	maxClientStates   int
	stateTTL          time.Duration
	trustedProxyCIDRs []netip.Prefix
}

func clientGuardConfigFromEnv() (clientGuardConfig, error) {
	rateLimit := envFloat("DOH_RATE_LIMIT", defaultDoHRateLimit, 0.1, maxDoHRateLimit)
	rateBurst := envInt("DOH_RATE_BURST", defaultDoHRateBurst, 1, maxDoHRateBurst)
	globalRateLimit := envFloat("GLOBAL_RATE_LIMIT", defaultGlobalRateLimit, 0.1, maxGlobalRateLimit)
	globalRateBurst := envInt("GLOBAL_RATE_BURST", defaultGlobalRateBurst, 1, maxGlobalRateBurst)
	maxIPConns := envInt("IP_CONN_LIMIT", defaultIPConnLimit, 1, maxIPConnLimit)
	maxIPRequests := envInt("DOH_MAX_IP_REQUESTS", defaultDoHIPRequests, 1, maxDoHIPRequests)
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
		rateLimit:         rateLimit,
		rateBurst:         rateBurst,
		globalRateLimit:   globalRateLimit,
		globalRateBurst:   globalRateBurst,
		maxIPConns:        maxIPConns,
		maxIPRequests:     maxIPRequests,
		maxClientStates:   maxStates,
		stateTTL:          defaultDoHStateTTL,
		trustedProxyCIDRs: cidrs,
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
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
	ipConns    int
	requests   int
}

type clientShard struct {
	mu    sync.Mutex
	items map[clientKey]*clientState
}

// clientGuard uses fixed-size shards and a hard per-shard entry cap. This keeps
// memory bounded even when an attacker cycles through huge numbers of source IPs.
// Eviction only removes idle entries, so active connection/request accounting is
// never lost underneath a live client.
type clientGuard struct {
	cfg              clientGuardConfig
	shards           [clientGuardShardCount]clientShard
	shardCap         int
	globalMu         sync.Mutex
	globalTokens     float64
	globalLastRefill time.Time
}

func newClientGuard(cfg clientGuardConfig) *clientGuard {
	if cfg.rateLimit <= 0 {
		cfg.rateLimit = defaultDoHRateLimit
	}
	if cfg.rateBurst <= 0 {
		cfg.rateBurst = defaultDoHRateBurst
	}
	if cfg.globalRateLimit <= 0 {
		cfg.globalRateLimit = defaultGlobalRateLimit
	}
	if cfg.globalRateBurst <= 0 {
		cfg.globalRateBurst = defaultGlobalRateBurst
	}
	if cfg.maxIPConns <= 0 {
		cfg.maxIPConns = defaultIPConnLimit
	}
	if cfg.maxIPRequests <= 0 {
		cfg.maxIPRequests = defaultDoHIPRequests
	}
	if cfg.maxClientStates < minDoHClientStates {
		cfg.maxClientStates = minDoHClientStates
	}
	cfg.maxClientStates = (cfg.maxClientStates / clientGuardShardCount) * clientGuardShardCount
	if cfg.stateTTL <= 0 {
		cfg.stateTTL = defaultDoHStateTTL
	}
	g := &clientGuard{
		cfg:          cfg,
		shardCap:     cfg.maxClientStates / clientGuardShardCount,
		globalTokens: float64(cfg.globalRateBurst),
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

func (g *clientGuard) getStateLocked(s *clientShard, key clientKey, now time.Time) (*clientState, bool) {
	if st, ok := s.items[key]; ok {
		st.lastSeen = now
		return st, true
	}

	if len(s.items) >= g.shardCap {
		var oldestKey clientKey
		var oldest *clientState
		var expiredKey clientKey
		var expired *clientState
		for candidateKey, candidate := range s.items {
			if candidate.ipConns != 0 || candidate.requests != 0 {
				continue
			}
			if !candidate.lastSeen.Add(g.cfg.stateTTL).After(now) &&
				(expired == nil || candidate.lastSeen.Before(expired.lastSeen)) {
				expiredKey = candidateKey
				expired = candidate
			}
			if oldest == nil || candidate.lastSeen.Before(oldest.lastSeen) {
				oldestKey = candidateKey
				oldest = candidate
			}
		}
		if expired != nil {
			delete(s.items, expiredKey)
		} else if oldest != nil {
			delete(s.items, oldestKey)
		} else {
			return nil, false
		}
	}

	st := &clientState{
		tokens:     float64(g.cfg.rateBurst),
		lastRefill: now,
		lastSeen:   now,
	}
	s.items[key] = st
	return st, true
}

func refillTokens(st *clientState, now time.Time, rate float64, burst int) {
	if now.Before(st.lastRefill) {
		return
	}
	elapsed := now.Sub(st.lastRefill).Seconds()
	if elapsed > 0 {
		st.tokens += elapsed * rate
		if st.tokens > float64(burst) {
			st.tokens = float64(burst)
		}
		st.lastRefill = now
	}
}

func (g *clientGuard) takeGlobalToken(now time.Time) bool {
	g.globalMu.Lock()
	defer g.globalMu.Unlock()

	if g.globalLastRefill.IsZero() {
		g.globalLastRefill = now
	} else if !now.Before(g.globalLastRefill) {
		elapsed := now.Sub(g.globalLastRefill).Seconds()
		g.globalTokens += elapsed * g.cfg.globalRateLimit
		if g.globalTokens > float64(g.cfg.globalRateBurst) {
			g.globalTokens = float64(g.cfg.globalRateBurst)
		}
		g.globalLastRefill = now
	}
	if g.globalTokens < 1 {
		return false
	}
	g.globalTokens--
	return true
}

func (g *clientGuard) beginRequest(ip netip.Addr, now time.Time) bool {
	key := clientIPKey(ip)
	s := &g.shards[clientShardIndex(key)]
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := g.getStateLocked(s, key, now)
	if !ok || st.requests >= g.cfg.maxIPRequests {
		return false
	}
	refillTokens(st, now, g.cfg.rateLimit, g.cfg.rateBurst)
	if st.tokens < 1 {
		return false
	}
	if !g.takeGlobalToken(now) {
		return false
	}
	st.tokens--
	st.requests++
	st.lastSeen = now
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
		ip := remoteAddrIP(conn.RemoteAddr())
		if !l.reserveGlobal() {
			_ = conn.Close()
			continue
		}
		// A configured trusted reverse proxy multiplexes many real clients over
		// its connections, so the per-source-IP connection cap would throttle
		// every user at once. It still counts against the global connection
		// cap, and per-client limits are applied per request using the
		// forwarded client IP instead.
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
	ip            netip.Addr
	tracked       bool // counted against the per-source-IP connection cap
	globalRelease func()
	once          sync.Once
}

func (c *guardConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		if c.tracked {
			c.guard.closeConnection(c.ip, time.Now())
		}
		c.globalRelease()
	})
	return err
}

func remoteAddrIP(addr net.Addr) netip.Addr {
	if addr == nil {
		return netip.IPv4Unspecified()
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

// requestClientIP trusts X-Forwarded-For only when the immediate peer is in an
// explicitly configured trusted-proxy network. Walking the list from right to
// left prevents an untrusted client from simply prepending a spoofed address.
func requestClientIP(r *http.Request, trustedProxies []netip.Prefix) netip.Addr {
	remote := remoteAddrIPString(r.RemoteAddr)
	if len(trustedProxies) == 0 || !trustedProxyContains(trustedProxies, remote) {
		return remote
	}

	values := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(values) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(strings.TrimSpace(values[i]))
		if err != nil {
			continue
		}
		ip = ip.Unmap()
		if !trustedProxyContains(trustedProxies, ip) {
			return ip
		}
	}

	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		if ip, err := netip.ParseAddr(realIP); err == nil {
			return ip.Unmap()
		}
	}
	return remote
}

func remoteAddrIPString(addr string) netip.Addr {
	text := strings.TrimSpace(addr)
	if host, _, err := net.SplitHostPort(text); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil {
			return ip.Unmap()
		}
	}
	if ip, err := netip.ParseAddr(text); err == nil {
		return ip.Unmap()
	}
	return netip.IPv4Unspecified()
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
