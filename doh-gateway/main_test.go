package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stateCount reports the number of tracked client states. It is only needed to
// assert the memory bound, so it lives with the tests rather than in the
// production guard.
func (g *clientGuard) stateCount() int {
	total := 0
	for i := range g.shards {
		g.shards[i].mu.Lock()
		total += len(g.shards[i].items)
		g.shards[i].mu.Unlock()
	}
	return total
}

func TestDecodeQuery(t *testing.T) {
	want := []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	encoded := base64.RawURLEncoding.EncodeToString(want)
	got, err := decodeQuery(encoded)
	if err != nil {
		t.Fatalf("decodeQuery() error = %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("decodeQuery() = %x, want %x", got, want)
	}
}

func TestValidateDNSQuery(t *testing.T) {
	valid := []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	if err := validateDNSQuery(valid); err != nil {
		t.Fatalf("validateDNSQuery(valid) error = %v", err)
	}

	short := make([]byte, 11)
	if err := validateDNSQuery(short); err == nil {
		t.Fatal("validateDNSQuery(short) expected an error")
	}

	response := append([]byte(nil), valid...)
	response[2] |= 0x80
	if err := validateDNSQuery(response); err == nil {
		t.Fatal("validateDNSQuery(response) expected an error")
	}
}

func TestFixedDNSUpstream(t *testing.T) {
	if fixedDNSUpstream != "127.0.0.1:5300" {
		t.Fatalf("fixed DNS upstream = %q, want 127.0.0.1:5300", fixedDNSUpstream)
	}
}

func TestDoHConcurrencyAndTimeoutDefaultsStayCapped(t *testing.T) {
	transport := defaultDNSTransportLimits()

	if _, got, _ := effectiveDoHLimits(maxDNSPacket, 100, transport); got != maxMaxInflight {
		t.Fatalf("DOH_MAX_INFLIGHT above cap normalized to %d, want %d", got, maxMaxInflight)
	}
	if _, got, _ := effectiveDoHLimits(maxDNSPacket, 0, transport); got != defaultMaxInflight {
		t.Fatalf("DOH_MAX_INFLIGHT below minimum normalized to %d, want %d", got, defaultMaxInflight)
	}
	if maxMaxInflight != 64 || defaultMaxInflight != 64 {
		t.Fatalf("DoH inflight cap/default changed: cap=%d default=%d, want 64/64", maxMaxInflight, defaultMaxInflight)
	}
	if defaultDoHIdleTimeout != 120*time.Second {
		t.Fatalf("default DoH idle timeout = %s, want 120s", defaultDoHIdleTimeout)
	}
}

func TestTunedDefaults(t *testing.T) {
	cfg, err := clientGuardConfigFromEnv()
	if err != nil {
		t.Fatalf("clientGuardConfigFromEnv() error = %v", err)
	}
	if cfg.maxIPConns != defaultIPConnLimit {
		t.Fatalf("default per-IP connections = %d, want %d", cfg.maxIPConns, defaultIPConnLimit)
	}
	if cfg.maxIPRequests != defaultDoHIPRequests {
		t.Fatalf("default per-IP requests = %d, want %d", cfg.maxIPRequests, defaultDoHIPRequests)
	}
	if cfg.maxClientStates != defaultDoHClientStates {
		t.Fatalf("default client states = %d, want %d", cfg.maxClientStates, defaultDoHClientStates)
	}
	if defaultIPConnLimit != 16 || defaultDoHIPRequests != 16 {
		t.Fatalf("public per-client defaults = %d connections / %d requests, want 16 / 16", defaultIPConnLimit, defaultDoHIPRequests)
	}
	if defaultMaxInflight != 64 || defaultMaxConns != 512 {
		t.Fatalf("public concurrency defaults = %d in-flight / %d connections, want 64 / 512", defaultMaxInflight, defaultMaxConns)
	}
	if defaultMaxConns > maxMaxConns {
		t.Fatalf("default connections = %d exceeds hard maximum %d", defaultMaxConns, maxMaxConns)
	}
}

func TestBindHostRejectsHostname(t *testing.T) {
	t.Setenv("DOH_BIND", "localhost")
	if _, err := bindHostFromEnv(); err == nil {
		t.Fatal("hostname DOH_BIND was accepted; binding must not perform hostname resolution")
	}

	t.Setenv("DOH_BIND", "127.0.0.1")
	got, err := bindHostFromEnv()
	if err != nil || got != "127.0.0.1" {
		t.Fatalf("IPv4 DOH_BIND = %q, err=%v", got, err)
	}
}

func TestServerTimeoutDefaultsAndEnv(t *testing.T) {
	t.Setenv("SERVER_TIMEOUT", "")
	if got := serverTimeoutFromEnv(); got != 6*time.Second {
		t.Fatalf("default SERVER_TIMEOUT = %s, want 6s", got)
	}
	t.Setenv("SERVER_TIMEOUT", "9")
	if got := serverTimeoutFromEnv(); got != 9*time.Second {
		t.Fatalf("SERVER_TIMEOUT=9 parsed as %s, want 9s", got)
	}
	t.Setenv("SERVER_TIMEOUT", "999")
	if got := serverTimeoutFromEnv(); got != 60*time.Second {
		t.Fatalf("SERVER_TIMEOUT over max parsed as %s, want 60s", got)
	}
}

func TestGuardEnvVars(t *testing.T) {
	for _, key := range []string{"IP_CONN_LIMIT", "DOH_MAX_IP_REQUESTS", "DOH_MAX_CLIENT_STATES"} {
		t.Setenv(key, "")
	}
	t.Setenv("IP_CONN_LIMIT", "8")
	t.Setenv("DOH_MAX_IP_REQUESTS", "12")
	t.Setenv("DOH_MAX_CLIENT_STATES", "34")
	cfg, err := clientGuardConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.maxIPConns != 8 || cfg.maxIPRequests != 12 || cfg.maxClientStates != 32 {
		t.Fatalf("guard env values not applied: conns=%d requests=%d states=%d, want 8/12/32", cfg.maxIPConns, cfg.maxIPRequests, cfg.maxClientStates)
	}
}

func TestClientGuardConcurrencyAndConnections(t *testing.T) {
	g := newClientGuard(clientGuardConfig{
		maxIPConns:      1,
		maxIPRequests:   1,
		maxClientStates: 16,
		stateTTL:        5 * time.Minute,
	})
	ip := netip.MustParseAddr("198.51.100.10")
	t0 := time.Unix(0, 0)

	if !g.beginRequest(ip, t0) {
		t.Fatal("first request was rejected")
	}
	if g.beginRequest(ip, t0) {
		t.Fatal("per-IP concurrent request limit was not enforced")
	}
	g.endRequest(ip, t0)
	if !g.beginRequest(ip, t0) {
		t.Fatal("request after releasing per-IP slot was rejected")
	}
	g.endRequest(ip, t0)

	if !g.openConnection(ip, t0) {
		t.Fatal("first connection was rejected")
	}
	if g.openConnection(ip, t0) {
		t.Fatal("per-IP connection limit was not enforced")
	}
	g.closeConnection(ip, t0)
	if !g.openConnection(ip, t0) {
		t.Fatal("connection after releasing per-IP slot was rejected")
	}
	g.closeConnection(ip, t0)
}

func TestClientGuardAllowsBurstUpToConcurrencyCap(t *testing.T) {
	g := newClientGuard(clientGuardConfig{
		maxIPConns:      8,
		maxIPRequests:   8,
		maxClientStates: 16,
		stateTTL:        5 * time.Minute,
	})
	ip := netip.MustParseAddr("198.51.100.30")
	now := time.Unix(0, 0)
	for i := 0; i < 8; i++ {
		if !g.beginRequest(ip, now) {
			t.Fatalf("request %d was rejected before the per-IP concurrency cap", i+1)
		}
	}
	if g.beginRequest(ip, now) {
		t.Fatal("request beyond the per-IP concurrency cap was accepted")
	}
	for i := 0; i < 8; i++ {
		g.endRequest(ip, now)
	}
}

func TestClientGuardStateIsBounded(t *testing.T) {
	cfg := clientGuardConfig{
		maxIPConns:      1,
		maxIPRequests:   1,
		maxClientStates: 16,
		stateTTL:        5 * time.Minute,
	}
	g := newClientGuard(cfg)
	now := time.Unix(0, 0)

	for i := 0; i < 5000; i++ {
		ip := netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)})
		if !g.beginRequest(ip, now) {
			continue
		}
		g.endRequest(ip, now)
	}
	if got := g.stateCount(); got > cfg.maxClientStates {
		t.Fatalf("client state count = %d, want <= %d", got, cfg.maxClientStates)
	}
}

func TestDefaultClientGuardStateIsBounded(t *testing.T) {
	cfg := clientGuardConfig{
		maxIPConns:      defaultIPConnLimit,
		maxIPRequests:   defaultDoHIPRequests,
		maxClientStates: defaultDoHClientStates,
		stateTTL:        defaultDoHStateTTL,
	}
	g := newClientGuard(cfg)
	now := time.Unix(0, 0)

	for i := 0; i < cfg.maxClientStates+1024; i++ {
		ip := netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)})
		if !g.beginRequest(ip, now) {
			continue
		}
		g.endRequest(ip, now)
	}
	if got := g.stateCount(); got > cfg.maxClientStates {
		t.Fatalf("default client state count = %d, want <= %d", got, cfg.maxClientStates)
	}
}

func TestGuardListenerImmediateCloseWhenGlobalFull(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := clientGuardConfig{
		maxIPConns:      4,
		maxIPRequests:   4,
		maxClientStates: 16,
		stateTTL:        5 * time.Minute,
	}
	guard := newClientGuard(cfg)
	ln := newGuardListener(base, guard, 1)
	defer ln.Close()

	firstClient, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer firstClient.Close()
	firstAccepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer firstAccepted.Close()

	secondClient, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer secondClient.Close()

	acceptDone := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		acceptDone <- err
	}()

	if err := secondClient.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := secondClient.Read(one[:]); err == nil {
		t.Fatal("second connection stayed open while the global limit was full")
	}

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acceptDone:
		if err == nil {
			t.Fatal("Accept returned nil after listener shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("Accept did not unblock after listener shutdown")
	}
}

func TestRequestClientIPTrustsOnlyConfiguredProxy(t *testing.T) {
	prefix := netip.MustParsePrefix("192.0.2.0/24")
	req := httptest.NewRequest(http.MethodGet, "http://example.test/dns-query", nil)
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 192.0.2.11")
	if got := requestClientIP(req, []netip.Prefix{prefix}); got.String() != "198.51.100.7" {
		t.Fatalf("trusted proxy client IP = %s, want 198.51.100.7", got)
	}

	req.RemoteAddr = "203.0.113.10:12345"
	if got := requestClientIP(req, []netip.Prefix{prefix}); got.String() != "203.0.113.10" {
		t.Fatalf("untrusted peer accepted forwarded IP %s", got)
	}
}

func TestGuardedTCPDNSConnEnforcesQueryCountAndFrameSize(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	response := []byte{0, 1, 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}
	go func() {
		var hdr [2]byte
		if _, err := io.ReadFull(server, hdr[:]); err != nil {
			return
		}
		n := int(hdr[0])<<8 | int(hdr[1])
		q := make([]byte, n)
		if _, err := io.ReadFull(server, q); err != nil {
			return
		}
		frame := append([]byte{byte(len(response) >> 8), byte(len(response))}, response...)
		_, _ = server.Write(frame)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tcpConn := guardedTCPDNSConn{conn: client, maxFrame: 64, maxQueries: 1}
	query := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	got, err := tcpConn.exchange(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, response) {
		t.Fatalf("guarded TCP response = %x, want %x", got, response)
	}
	if _, err := tcpConn.exchange(ctx, query); !errors.Is(err, errTCPQueriesExceeded) {
		t.Fatalf("second TCP query error = %v, want %v", err, errTCPQueriesExceeded)
	}

	large := make([]byte, 65)
	if _, err := (&guardedTCPDNSConn{conn: client, maxFrame: 64, maxQueries: 1}).exchange(ctx, large); err == nil {
		t.Fatal("oversized TCP query was accepted")
	}
}

func TestDNSExchangeUDPRejectsOversizedQueryBeforeDial(t *testing.T) {
	query := make([]byte, 129)
	_, err := dnsExchangeUDP(context.Background(), "not-a-valid-udp-address", query, 128)
	if err == nil || !strings.Contains(err.Error(), "UDP packet limit") {
		t.Fatalf("dnsExchangeUDP() error = %v, want UDP packet limit validation before dial", err)
	}
}

func TestDNSExchangeUDPDropsOversizedDatagram(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	addr := udp.LocalAddr().String()

	query := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	want := append([]byte(nil), query...)
	want[2] |= 0x80
	go func() {
		buf := make([]byte, maxDNSPacket)
		n, client, err := udp.ReadFromUDP(buf)
		if err != nil || n != len(query) {
			return
		}
		oversized := make([]byte, 129)
		_, _ = udp.WriteToUDP(oversized, client)
		_, _ = udp.WriteToUDP(want, client)
	}()

	got, err := dnsExchangeUDP(context.Background(), addr, query, 128)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("UDP response after overflow = %x, want %x", got, want)
	}
}

func TestDNSExchangeTruncatedUDPFallsBackToTCP(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()

	port := udp.LocalAddr().(*net.UDPAddr).Port
	tcp, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()

	want := []byte{0, 1, 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}

	go func() {
		buf := make([]byte, maxDNSPacket)
		_, addr, err := udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		truncated := append([]byte(nil), want...)
		truncated[2] |= 0x02
		_, _ = udp.WriteToUDP(truncated, addr)
	}()

	go func() {
		conn, err := tcp.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var hdr [2]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return
		}
		n := int(hdr[0])<<8 | int(hdr[1])
		q := make([]byte, n)
		if _, err := io.ReadFull(conn, q); err != nil {
			return
		}
		frame := append([]byte{byte(len(want) >> 8), byte(len(want))}, want...)
		_, _ = conn.Write(frame)
	}()

	query := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	got, err := dnsExchangeWithLimits(context.Background(), "127.0.0.1:"+strconv.Itoa(port), query, defaultDNSTransportLimits())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("dnsExchangeWithLimits() = %x, want %x", got, want)
	}
}

func TestDNSExchangeUDPResponseIsNotAliasedToPooledBuffer(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	addr := udp.LocalAddr().String()

	// Echo server: the response is the query, so each exchange is identifiable.
	go func() {
		buf := make([]byte, maxDNSPacket)
		for {
			n, client, err := udp.ReadFromUDP(buf)
			if err != nil {
				return
			}
			buf[2] |= 0x80
			_, _ = udp.WriteToUDP(buf[:n], client)
		}
	}()

	q1 := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	q2 := []byte{0, 2, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}

	first, err := dnsExchangeWithLimits(context.Background(), addr, q1, defaultDNSTransportLimits())
	if err != nil {
		t.Fatal(err)
	}
	// A second exchange reuses the pooled receive buffer; it must not
	// overwrite the response already returned by the first.
	if _, err := dnsExchangeWithLimits(context.Background(), addr, q2, defaultDNSTransportLimits()); err != nil {
		t.Fatal(err)
	}
	wantFirst := append([]byte(nil), q1...)
	wantFirst[2] |= 0x80
	if !bytes.Equal(first, wantFirst) {
		t.Fatalf("first response changed after a second exchange: got %x, want %x", first, wantFirst)
	}
}

func TestDNSExchangeUDPTimeoutDoesNotFallBackToTCP(t *testing.T) {
	// A UDP socket that never answers. Once the shared budget is spent the
	// exchange must fail with the UDP error, not with a follow-on TCP error.
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	query := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	start := time.Now()
	_, err = dnsExchangeWithLimits(ctx, udp.LocalAddr().String(), query, defaultDNSTransportLimits())
	if err == nil {
		t.Fatal("dnsExchangeWithLimits() expected an error from an unresponsive upstream")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("dnsExchangeWithLimits() took %s, want it bounded by the context deadline", elapsed)
	}
	if strings.Contains(err.Error(), "dial tcp") {
		t.Fatalf("dnsExchangeWithLimits() error = %v, want the UDP error, not a TCP dial error", err)
	}
}

func TestHTTPWriteTimeoutHasHeadroomOverDNSExchangeTimeout(t *testing.T) {
	if httpWriteTimeout <= defaultServerTimeout {
		t.Fatalf("httpWriteTimeout (%s) must be greater than defaultServerTimeout (%s)", httpWriteTimeout, defaultServerTimeout)
	}
}

func TestDNSResponseValidation(t *testing.T) {
	query := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	valid := append([]byte(nil), query...)
	valid[2] |= 0x80
	if err := validateDNSResponse(query, valid); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}

	badID := append([]byte(nil), valid...)
	badID[1]++
	if err := validateDNSResponse(query, badID); err == nil {
		t.Fatal("transaction ID mismatch was accepted")
	}

	notResponse := append([]byte(nil), valid...)
	notResponse[2] &^= 0x80
	if err := validateDNSResponse(query, notResponse); err == nil {
		t.Fatal("response without QR bit was accepted")
	}

	opcodeMismatch := append([]byte(nil), valid...)
	opcodeMismatch[2] |= 0x08
	if err := validateDNSResponse(query, opcodeMismatch); err == nil {
		t.Fatal("response with opcode mismatch was accepted")
	}
}

func TestDoHHandlerMapsInvalidBackendResponseTo502(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()

	go func() {
		buf := make([]byte, maxDNSPacket)
		n, client, err := udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		bad := append([]byte(nil), buf[:n]...)
		bad[2] &^= 0x80
		_, _ = udp.WriteToUDP(bad, client)
	}()

	query := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	h := dohHandlerWithGuard(udp.LocalAddr().String(), "/dns-query", maxDNSPacket, 1, defaultDNSTransportLimits(), nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/dns-query", bytes.NewReader(query))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("invalid backend response status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestDoHHandlerClientDisconnectIsSilent(t *testing.T) {
	var logs strings.Builder
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldWriter)

	query := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	h := dohHandlerWithGuard("127.0.0.1:1", "/dns-query", maxDNSPacket, 1, defaultDNSTransportLimits(), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/dns-query", bytes.NewReader(query)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("canceled request status = %d, want zero-value recorder status %d", rec.Code, http.StatusOK)
	}
	if got := logs.String(); got != "" {
		t.Fatalf("client disconnect produced log output: %q", got)
	}
}

func TestDoHHandlerMapsBackendTimeoutTo502(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()

	query := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	h := dohHandlerWithGuardTimeout(udp.LocalAddr().String(), "/dns-query", maxDNSPacket, 1, defaultDNSTransportLimits(), nil, nil, 50*time.Millisecond)
	req := httptest.NewRequest(http.MethodPost, "/dns-query", bytes.NewReader(query))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("backend timeout status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestDoHHandlerGetStandardBase64Plus(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()

	addr := udp.LocalAddr().String()
	go func() {
		for range 2 {
			buf := make([]byte, maxDNSPacket)
			n, client, err := udp.ReadFromUDP(buf)
			if err != nil {
				return
			}
			buf[2] |= 0x80
			_, _ = udp.WriteToUDP(buf[:n], client)
		}
	}()

	// 0xf8 in the message makes the standard Base64 representation contain '+'.
	query := []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xf8}
	want := append([]byte(nil), query...)
	want[2] |= 0x80
	encoded := base64.StdEncoding.EncodeToString(query)
	if !strings.Contains(encoded, "+") {
		t.Fatalf("test fixture must contain '+': %q", encoded)
	}

	h := dohHandlerWithGuard(addr, "/dns-query", maxDNSPacket, 1, defaultDNSTransportLimits(), nil, nil)
	for name, value := range map[string]string{
		"literal plus": encoded,
		"escaped plus": strings.ReplaceAll(encoded, "+", "%2B"),
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/dns-query?dns="+value, nil)
			rec := httptest.NewRecorder()
			h(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("standard Base64 GET status = %d, want %d", rec.Code, http.StatusOK)
			}
			if got := rec.Body.Bytes(); !bytes.Equal(got, want) {
				t.Fatalf("standard Base64 GET response = %x, want %x", got, want)
			}
		})
	}
}

func TestDoHServerGetOverMaxBodyReachesHandler(t *testing.T) {
	const maxBody = 8 << 10

	h := dohHandlerWithGuard("127.0.0.1:1", "/dns-query", maxBody, 1, defaultDNSTransportLimits(), nil, nil)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.MaxHeaderBytes = maxHeaderBytesFor(maxBody)
	srv.Start()
	defer srv.Close()

	query := make([]byte, maxBody+1)
	encoded := base64.RawURLEncoding.EncodeToString(query)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/dns-query?dns="+encoded, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("GET over maxBody through net/http status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

func TestDoHServerGetPercentEscapedStandardBase64OverMaxBodyReachesHandler(t *testing.T) {
	const maxBody = 8 << 10

	h := dohHandlerWithGuard("127.0.0.1:1", "/dns-query", maxBody, 1, defaultDNSTransportLimits(), nil, nil)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.MaxHeaderBytes = maxHeaderBytesFor(maxBody)
	srv.Start()
	defer srv.Close()

	// 0xff bytes encode entirely as '/' in standard Base64, making the
	// percent-escaped representation close to the worst supported GET size.
	query := bytes.Repeat([]byte{0xff}, maxBody+1)
	encoded := base64.StdEncoding.EncodeToString(query)
	escaped := strings.ReplaceAll(encoded, "/", "%2F")
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/dns-query?dns="+escaped, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("percent-escaped standard Base64 GET over maxBody status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

func TestDoHHandlerGetRejectsQueryOverMaxBody(t *testing.T) {
	const smallMaxBody = 16
	h := dohHandlerWithGuard("127.0.0.1:1", "/dns-query", smallMaxBody, 1, defaultDNSTransportLimits(), nil, nil)

	query := make([]byte, smallMaxBody+1)
	query[2] = 0
	encoded := base64.RawURLEncoding.EncodeToString(query)
	req := httptest.NewRequest("GET", "/dns-query?dns="+encoded, nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("GET over maxBody status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestDoHHandlerRejectsDNSResponseMessage(t *testing.T) {
	query := []byte{0, 2, 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}
	h := dohHandlerWithGuard("127.0.0.1:1", "/dns-query", maxDNSPacket, 1, defaultDNSTransportLimits(), nil, nil)

	postReq := httptest.NewRequest("POST", "/dns-query", bytes.NewReader(query))
	postRec := httptest.NewRecorder()
	h(postRec, postReq)

	if postRec.Code != http.StatusBadRequest {
		t.Fatalf("response-as-query status = %d, want %d", postRec.Code, http.StatusBadRequest)
	}
}

func TestDoHHandlerGetAndPost(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	addr := "127.0.0.1:" + strconv.Itoa(udp.LocalAddr().(*net.UDPAddr).Port)

	go func() {
		for range 2 {
			buf := make([]byte, maxDNSPacket)
			n, client, err := udp.ReadFromUDP(buf)
			if err != nil {
				return
			}
			buf[2] |= 0x80
			_, _ = udp.WriteToUDP(buf[:n], client)
		}
	}()

	query := []byte{0, 2, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	h := dohHandlerWithGuard(addr, "/dns-query", maxDNSPacket, 1, defaultDNSTransportLimits(), nil, nil)

	postReq := httptest.NewRequest("POST", "/dns-query", bytes.NewReader(query))
	postReq.Header.Set("Content-Type", "application/dns-message")
	postRec := httptest.NewRecorder()
	h(postRec, postReq)
	if postRec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want %d", postRec.Code, http.StatusOK)
	}
	if got := postRec.Header().Get("Content-Type"); got != "application/dns-message" {
		t.Fatalf("POST Content-Type = %q", got)
	}

	encoded := base64.RawURLEncoding.EncodeToString(query)
	getReq := httptest.NewRequest("GET", "/dns-query?dns="+encoded, nil)
	getRec := httptest.NewRecorder()
	h(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", getRec.Code, http.StatusOK)
	}
	if got := getRec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("GET CORS origin = %q", got)
	}
}

func TestDoHHandlerAppliesPerIPConcurrencyCap(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	addr := udp.LocalAddr().String()

	query := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	requestSeen := make(chan struct{})
	go func() {
		buf := make([]byte, maxDNSPacket)
		_, _, err := udp.ReadFromUDP(buf)
		if err == nil {
			close(requestSeen)
		}
	}()

	guard := newClientGuard(clientGuardConfig{
		maxIPConns:      8,
		maxIPRequests:   1,
		maxClientStates: 16,
		stateTTL:        5 * time.Minute,
	})
	h := dohHandlerWithGuardTimeout(addr, "/dns-query", maxDNSPacket, 2, defaultDNSTransportLimits(), guard, nil, 5*time.Second)

	encoded := base64.RawURLEncoding.EncodeToString(query)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest(http.MethodGet, "/dns-query?dns="+encoded, nil).WithContext(ctx)
		req.RemoteAddr = "198.51.100.20:1234"
		rec := httptest.NewRecorder()
		h(rec, req)
	}()

	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach the DNS backend")
	}

	second := httptest.NewRequest(http.MethodGet, "/dns-query?dns="+encoded, nil)
	second.RemoteAddr = "198.51.100.20:5678"
	second.Host = "other.example.test"
	rec := httptest.NewRecorder()
	h(rec, second)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("second concurrent request status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	cancel()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first request did not stop after cancellation")
	}
}

func TestReadyEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	h := dohHandlerWithGuard(ln.Addr().String(), "/dns-query", maxDNSPacket, 1, defaultDNSTransportLimits(), nil, nil)
	req := httptest.NewRequest("GET", "/readyz", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "ready\n" {
		t.Fatalf("readyz body = %q, want %q", got, "ready\n")
	}
}

func TestReadyEndpointUnavailable(t *testing.T) {
	// Reserve a free port, then release it so nothing is listening there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	h := dohHandlerWithGuard(addr, "/dns-query", maxDNSPacket, 1, defaultDNSTransportLimits(), nil, nil)
	req := httptest.NewRequest("GET", "/readyz", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := rec.Body.String(); got != "upstream unavailable\n" {
		t.Fatalf("readyz body = %q, want %q", got, "upstream unavailable\n")
	}
}

func TestGuardListenerTrustedProxyIsExemptFromPerIPConnectionCap(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guard := newClientGuard(clientGuardConfig{
		maxIPConns:        1,
		maxIPRequests:     4,
		maxClientStates:   16,
		stateTTL:          5 * time.Minute,
		trustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	})
	ln := newGuardListener(base, guard, 8)
	defer ln.Close()

	// A rejected connection would make Accept loop until this deadline.
	if err := base.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		client, err := net.Dial("tcp", base.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		accepted, err := ln.Accept()
		if err != nil {
			t.Fatalf("trusted proxy connection %d was rejected: %v", i+1, err)
		}
		defer accepted.Close()
	}
}

func TestGuardListenerEnforcesPerIPConnectionCapForUntrustedPeers(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guard := newClientGuard(clientGuardConfig{
		maxIPConns:      1,
		maxIPRequests:   4,
		maxClientStates: 16,
		stateTTL:        5 * time.Minute,
	})
	ln := newGuardListener(base, guard, 8)
	defer ln.Close()

	first, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	firstAccepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	second, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	// The second connection from the same IP is closed inside Accept, which
	// then keeps waiting; the listener deadline ends that wait.
	if err := base.(*net.TCPListener).SetDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if conn, err := ln.Accept(); err == nil {
		conn.Close()
		t.Fatal("second connection from the same untrusted IP was accepted")
	}

	// Closing the first connection releases the per-IP slot.
	_ = firstAccepted.Close()
	if err := base.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	third, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	thirdAccepted, err := ln.Accept()
	if err != nil {
		t.Fatalf("connection after releasing the per-IP slot was rejected: %v", err)
	}
	_ = thirdAccepted.Close()
}

func TestEnvIntTrimsWhitespaceAndClamps(t *testing.T) {
	t.Setenv("DOH_TEST_INT", " 42 ")
	if got := envInt("DOH_TEST_INT", 7, 1, 100); got != 42 {
		t.Fatalf("envInt(\" 42 \") = %d, want 42", got)
	}
	t.Setenv("DOH_TEST_INT", "99999999999999999999")
	if got := envInt("DOH_TEST_INT", 7, 1, 100); got != 100 {
		t.Fatalf("envInt(overflow) = %d, want clamp 100", got)
	}
	t.Setenv("DOH_TEST_INT", "-5")
	if got := envInt("DOH_TEST_INT", 7, 1, 100); got != 7 {
		t.Fatalf("envInt(-5) = %d, want fallback 7", got)
	}
	t.Setenv("DOH_TEST_INT", "0")
	if got := envInt("DOH_TEST_INT", 7, 1, 100); got != 7 {
		t.Fatalf("envInt(0) = %d, want fallback 7", got)
	}
}

func TestReadyEndpointCachesProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var accepts atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = conn.Close()
		}
	}()

	h := dohHandlerWithGuard(ln.Addr().String(), "/dns-query", maxDNSPacket, 1, defaultDNSTransportLimits(), nil, nil)
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("readyz call %d status = %d, want %d", i+1, rec.Code, http.StatusOK)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := accepts.Load(); got != 1 {
		t.Fatalf("5 /readyz calls within the cache TTL opened %d upstream connections, want 1", got)
	}
}
