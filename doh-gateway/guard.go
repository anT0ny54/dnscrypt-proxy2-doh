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
	defaultDoHRPS          = 10.0
	defaultDoHBurst        = 20
	defaultDoHIPConns      = 8
	defaultDoHIPRequests   = 8
	defaultDoHClientStates = 1024
	defaultDoHStateTTL     = 5 * time.Minute
	defaultMaxUDPPacket    = 8 << 10
	defaultMaxTCPFrame     = 8 << 10
	defaultMaxTCPQueries   = 1
	maxDoHRPS              = 100.0
	maxDoHBurst            = 256
	maxDoHIPConns          = 32
	maxDoHIPRequests       = 32
	maxDoHClientStates     = 2048
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
	rps               float64
	burst             int
	maxIPConns        int
	maxIPRequests     int
	maxClientStates   int
	stateTTL          time.Duration
	trustedProxyCIDRs []netip.Prefix
}

func clientGuardConfigFromEnv() (clientGuardConfig, error) {
	rps := envFloat("DOH_RATE_RPS", defaultDoHRPS, 0.1, maxDoHRPS)
	burst := envInt("DOH_RATE_BURST", defaultDoHBurst, 1, maxDoHBurst)
	maxIPConns := envInt("DOH_MAX_IP_CONNS", defaultDoHIPConns, 1, maxDoHIPConns)
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
		rps:               rps,
		burst:             burst,
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

type clientState struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
	ipConns    int
	requests   int
}

type clientShard struct {
	mu    sync.Mutex
	items map[netip.Addr]*clientState
}

// clientGuard uses fixed-size shards and a hard per-shard entry cap. This keeps
// memory bounded even when an attacker cycles through huge numbers of source IPs.
// Eviction only removes idle entries, so active connection/request accounting is
// never lost underneath a live client.
type clientGuard struct {
	cfg      clientGuardConfig
	shards   [clientGuardShardCount]clientShard
	shardCap int
}

func newClientGuard(cfg clientGuardConfig) *clientGuard {
	g := &clientGuard{
		cfg:      cfg,
		shardCap: cfg.maxClientStates / clientGuardShardCount,
	}
	for i := range g.shards {
		g.shards[i].items = make(map[netip.Addr]*clientState, g.shardCap)
	}
	return g
}

func clientShardIndex(ip netip.Addr) int {
	b := ip.As16()
	var h uint32 = 2166136261
	for _, x := range b {
		h ^= uint32(x)
		h *= 16777619
	}
	return int(h % clientGuardShardCount)
}

func (g *clientGuard) getStateLocked(s *clientShard, ip netip.Addr, now time.Time) (*clientState, bool) {
	if st, ok := s.items[ip]; ok {
		st.lastSeen = now
		return st, true
	}

	if len(s.items) >= g.shardCap {
		var oldestIP netip.Addr
		var oldest *clientState
		var expiredIP netip.Addr
		var expired *clientState
		for candidateIP, candidate := range s.items {
			if candidate.ipConns != 0 || candidate.requests != 0 {
				continue
			}
			if !candidate.lastSeen.Add(g.cfg.stateTTL).After(now) &&
				(expired == nil || candidate.lastSeen.Before(expired.lastSeen)) {
				expiredIP = candidateIP
				expired = candidate
			}
			if oldest == nil || candidate.lastSeen.Before(oldest.lastSeen) {
				oldestIP = candidateIP
				oldest = candidate
			}
		}
		if expired != nil {
			delete(s.items, expiredIP)
		} else if oldest != nil {
			delete(s.items, oldestIP)
		} else {
			return nil, false
		}
	}

	st := &clientState{
		tokens:     float64(g.cfg.burst),
		lastRefill: now,
		lastSeen:   now,
	}
	s.items[ip] = st
	return st, true
}

func refillTokens(st *clientState, now time.Time, rps float64, burst int) {
	if now.Before(st.lastRefill) {
		return
	}
	elapsed := now.Sub(st.lastRefill).Seconds()
	if elapsed > 0 {
		st.tokens += elapsed * rps
		if st.tokens > float64(burst) {
			st.tokens = float64(burst)
		}
		st.lastRefill = now
	}
}

func (g *clientGuard) beginRequest(ip netip.Addr, now time.Time) bool {
	s := &g.shards[clientShardIndex(ip)]
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := g.getStateLocked(s, ip, now)
	if !ok || st.requests >= g.cfg.maxIPRequests {
		return false
	}
	refillTokens(st, now, g.cfg.rps, g.cfg.burst)
	if st.tokens < 1 {
		return false
	}
	st.tokens--
	st.requests++
	st.lastSeen = now
	return true
}

func (g *clientGuard) endRequest(ip netip.Addr, now time.Time) {
	s := &g.shards[clientShardIndex(ip)]
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.items[ip]; ok {
		if st.requests > 0 {
			st.requests--
		}
		st.lastSeen = now
	}
}

func (g *clientGuard) openConnection(ip netip.Addr, now time.Time) bool {
	s := &g.shards[clientShardIndex(ip)]
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := g.getStateLocked(s, ip, now)
	if !ok || st.ipConns >= g.cfg.maxIPConns {
		return false
	}
	st.ipConns++
	st.lastSeen = now
	return true
}

func (g *clientGuard) closeConnection(ip netip.Addr, now time.Time) {
	s := &g.shards[clientShardIndex(ip)]
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.items[ip]; ok {
		if st.ipConns > 0 {
			st.ipConns--
		}
		st.lastSeen = now
	}
}

func (g *clientGuard) stateCount() int {
	total := 0
	for i := range g.shards {
		g.shards[i].mu.Lock()
		total += len(g.shards[i].items)
		g.shards[i].mu.Unlock()
	}
	return total
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
		if !l.guard.openConnection(ip, now) {
			l.releaseGlobal()
			_ = conn.Close()
			continue
		}

		return &guardConn{
			Conn:          conn,
			guard:         l.guard,
			ip:            ip,
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
	globalRelease func()
	once          sync.Once
}

func (c *guardConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.guard.closeConnection(c.ip, time.Now())
		c.globalRelease()
	})
	return err
}

func remoteAddrIP(addr net.Addr) netip.Addr {
	if addr == nil {
		return netip.IPv4Unspecified()
	}
	text := strings.TrimSpace(addr.String())
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

	setConnDeadline(ctx, c.conn)
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
