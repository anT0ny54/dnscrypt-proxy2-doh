package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

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

func TestTunedDefaults(t *testing.T) {
	for _, key := range []string{
		"DOH_RATE_RPS",
		"DOH_RATE_BURST",
		"DOH_MAX_IP_CONNS",
		"DOH_MAX_IP_REQUESTS",
		"DOH_MAX_CLIENT_STATES",
	} {
		t.Setenv(key, "")
	}

	cfg, err := clientGuardConfigFromEnv()
	if err != nil {
		t.Fatalf("clientGuardConfigFromEnv() error = %v", err)
	}
	if cfg.rps != defaultDoHRPS {
		t.Fatalf("default DoH RPS = %v, want %v", cfg.rps, defaultDoHRPS)
	}
	if cfg.burst != defaultDoHBurst {
		t.Fatalf("default DoH burst = %d, want %d", cfg.burst, defaultDoHBurst)
	}
	if cfg.maxIPConns != defaultDoHIPConns {
		t.Fatalf("default per-IP connections = %d, want %d", cfg.maxIPConns, defaultDoHIPConns)
	}
	if cfg.maxIPRequests != defaultDoHIPRequests {
		t.Fatalf("default per-IP requests = %d, want %d", cfg.maxIPRequests, defaultDoHIPRequests)
	}
	if cfg.maxClientStates != defaultDoHClientStates {
		t.Fatalf("default client states = %d, want %d", cfg.maxClientStates, defaultDoHClientStates)
	}
	if defaultDoHClientStates > maxDoHClientStates {
		t.Fatalf("default client states = %d exceeds hard maximum %d", defaultDoHClientStates, maxDoHClientStates)
	}
	if defaultMaxInflight != 64 || defaultMaxConns != 96 {
		t.Fatalf("public concurrency defaults = %d in-flight / %d connections, want 64 / 96", defaultMaxInflight, defaultMaxConns)
	}
}

func TestDefaultClientGuardStartupBurstAndSustainedRate(t *testing.T) {
	g := newClientGuard(clientGuardConfig{
		rps:             defaultDoHRPS,
		burst:           defaultDoHBurst,
		maxIPConns:      defaultDoHIPConns,
		maxIPRequests:   defaultDoHIPRequests,
		maxClientStates: 16,
		stateTTL:        defaultDoHStateTTL,
	})
	ip := netip.MustParseAddr("198.51.100.31")
	key := clientHostKey(ip, "dns.example.test")
	t0 := time.Unix(0, 0)

	if defaultDoHBurst != 80 {
		t.Fatalf("default startup burst = %d, want 80", defaultDoHBurst)
	}
	for i := 0; i < 80; i++ {
		if !g.beginRequest(key, t0) {
			t.Fatalf("startup burst request %d was rejected", i+1)
		}
		g.endRequest(key, t0)
	}
	if g.beginRequest(key, t0) {
		t.Fatal("request beyond the 80-request startup burst was accepted immediately")
	}

	// At exactly 100 requests / 60 seconds, one token refills every 600ms.
	refillAt := t0.Add(600 * time.Millisecond)
	if !g.beginRequest(key, refillAt) {
		t.Fatal("one-token sustained refill after 600ms was rejected")
	}
	g.endRequest(key, refillAt)
}

func TestClientGuardRateAndConcurrency(t *testing.T) {
	cfg := clientGuardConfig{
		rps:             1,
		burst:           2,
		maxIPConns:      1,
		maxIPRequests:   1,
		maxClientStates: 16,
		stateTTL:        5 * time.Minute,
	}
	g := newClientGuard(cfg)
	ip := netip.MustParseAddr("198.51.100.10")
	t0 := time.Unix(0, 0)

	if !g.beginRequest(clientIPKey(ip), t0) {
		t.Fatal("first request was rejected")
	}
	if g.beginRequest(clientIPKey(ip), t0) {
		t.Fatal("per-IP concurrent request limit was not enforced")
	}
	g.endRequest(clientIPKey(ip), t0)
	if !g.beginRequest(clientIPKey(ip), t0) {
		t.Fatal("second request should consume the burst token")
	}
	g.endRequest(clientIPKey(ip), t0)
	if g.beginRequest(clientIPKey(ip), t0) {
		t.Fatal("token bucket allowed a third immediate request")
	}
	if !g.beginRequest(clientIPKey(ip), t0.Add(time.Second)) {
		t.Fatal("token bucket did not refill after one second")
	}
	g.endRequest(clientIPKey(ip), t0.Add(time.Second))

	if !g.openConnection(ip, t0) {
		t.Fatal("first connection was rejected")
	}
	if g.openConnection(ip, t0) {
		t.Fatal("per-IP connection limit was not enforced")
	}
	g.closeConnection(ip, t0)
}

func TestClientGuardRateIsPerIPAndHost(t *testing.T) {
	cfg := clientGuardConfig{
		rps:             1,
		burst:           1,
		maxIPConns:      1,
		maxIPRequests:   8,
		maxClientStates: 32,
		stateTTL:        5 * time.Minute,
	}
	g := newClientGuard(cfg)
	ip := netip.MustParseAddr("198.51.100.30")
	now := time.Unix(0, 0)

	a := clientHostKey(ip, "DNS.Example.test:443")
	b := clientHostKey(ip, "other.example.test")
	if !g.beginRequest(a, now) {
		t.Fatal("first request for host A was rejected")
	}
	g.endRequest(a, now)
	if g.beginRequest(a, now) {
		t.Fatal("second immediate request for the same IP + host should be rejected")
	}
	if !g.beginRequest(b, now) {
		t.Fatal("same IP on a different host should have a separate rate bucket")
	}
	g.endRequest(b, now)

	if clientHostKey(ip, "dns.example.test") != a {
		t.Fatal("host normalization did not collapse case/port differences")
	}

	if !g.openConnection(ip, now) {
		t.Fatal("first per-IP connection was rejected")
	}
	if g.openConnection(ip, now) {
		t.Fatal("per-IP connection cap was not aggregated across hosts")
	}
	g.closeConnection(ip, now)
}

func TestClientGuardStateIsBounded(t *testing.T) {
	cfg := clientGuardConfig{
		rps:             10,
		burst:           10,
		maxIPConns:      1,
		maxIPRequests:   1,
		maxClientStates: 16,
		stateTTL:        5 * time.Minute,
	}
	g := newClientGuard(cfg)
	now := time.Unix(0, 0)

	for i := 0; i < 5000; i++ {
		ip := netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)})
		if !g.beginRequest(clientIPKey(ip), now) {
			// A shard can be temporarily full of the same active entry, but the
			// request is immediately released in this test so state remains bounded.
			continue
		}
		g.endRequest(clientIPKey(ip), now)
	}
	if got := g.stateCount(); got > cfg.maxClientStates {
		t.Fatalf("client state count = %d, want <= %d", got, cfg.maxClientStates)
	}
}

func TestDefaultClientGuardStateIsBounded(t *testing.T) {
	cfg := clientGuardConfig{
		rps:             defaultDoHRPS,
		burst:           defaultDoHBurst,
		maxIPConns:      defaultDoHIPConns,
		maxIPRequests:   defaultDoHIPRequests,
		maxClientStates: defaultDoHClientStates,
		stateTTL:        defaultDoHStateTTL,
	}
	g := newClientGuard(cfg)
	now := time.Unix(0, 0)

	for i := 0; i < cfg.maxClientStates+1024; i++ {
		ip := netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)})
		key := clientHostKey(ip, "dns.example.test")
		if !g.beginRequest(key, now) {
			continue
		}
		g.endRequest(key, now)
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
		rps:             10,
		burst:           10,
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
	if !bytes.Equal(first, q1) {
		t.Fatalf("first response changed after a second exchange: got %x, want %x", first, q1)
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
	if httpWriteTimeout <= dnsExchangeTimeout {
		t.Fatalf("httpWriteTimeout (%s) must be greater than dnsExchangeTimeout (%s)", httpWriteTimeout, dnsExchangeTimeout)
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
			_, _ = udp.WriteToUDP(buf[:n], client)
		}
	}()

	// 0xf8 in the message makes the standard Base64 representation contain '+'.
	query := []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xf8}
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
			if got := rec.Body.Bytes(); !bytes.Equal(got, query) {
				t.Fatalf("standard Base64 GET response = %x, want %x", got, query)
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

func TestDoHHandlerAppliesClientGuard(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	addr := udp.LocalAddr().String()

	query := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	go func() {
		for range 2 {
			buf := make([]byte, maxDNSPacket)
			n, client, err := udp.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = udp.WriteToUDP(buf[:n], client)
		}
	}()

	guard := newClientGuard(clientGuardConfig{
		rps:             1,
		burst:           1,
		maxIPConns:      8,
		maxIPRequests:   8,
		maxClientStates: 16,
		stateTTL:        5 * time.Minute,
	})
	h := dohHandlerWithGuard(addr, "/dns-query", maxDNSPacket, 1, defaultDNSTransportLimits(), guard, nil)
	encoded := base64.RawURLEncoding.EncodeToString(query)

	req1 := httptest.NewRequest(http.MethodGet, "/dns-query?dns="+encoded, nil)
	req1.RemoteAddr = "198.51.100.20:1234"
	rec1 := httptest.NewRecorder()
	h(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first guarded request status = %d, want %d", rec1.Code, http.StatusOK)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/dns-query?dns="+encoded, nil)
	req2.RemoteAddr = "198.51.100.20:5678"
	rec2 := httptest.NewRecorder()
	h(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second guarded request status = %d, want %d", rec2.Code, http.StatusTooManyRequests)
	}

	req3 := httptest.NewRequest(http.MethodGet, "/dns-query?dns="+encoded, nil)
	req3.RemoteAddr = "198.51.100.20:5678"
	req3.Host = "other.example.test"
	rec3 := httptest.NewRecorder()
	h(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("different-host guarded request status = %d, want %d", rec3.Code, http.StatusOK)
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
