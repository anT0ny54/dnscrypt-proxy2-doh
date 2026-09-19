package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

func TestLimitListenerShutdownWhenFull(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newLimitListener(base, 1)

	client, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	accepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()

	acceptDone := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		acceptDone <- err
	}()

	time.Sleep(25 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-acceptDone:
		if err == nil {
			t.Fatal("blocked Accept unexpectedly returned a connection")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Accept did not unblock after listener shutdown")
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
	got, err := dnsExchange(context.Background(), "127.0.0.1:"+strconv.Itoa(port), query)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("dnsExchange() = %x, want %x", got, want)
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

	first, err := dnsExchange(context.Background(), addr, q1)
	if err != nil {
		t.Fatal(err)
	}
	// A second exchange reuses the pooled receive buffer; it must not
	// overwrite the response already returned by the first.
	if _, err := dnsExchange(context.Background(), addr, q2); err != nil {
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
	_, err = dnsExchange(ctx, udp.LocalAddr().String(), query)
	if err == nil {
		t.Fatal("dnsExchange() expected an error from an unresponsive upstream")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("dnsExchange() took %s, want it bounded by the context deadline", elapsed)
	}
	if strings.Contains(err.Error(), "dial tcp") {
		t.Fatalf("dnsExchange() error = %v, want the UDP error, not a TCP dial error", err)
	}
}

func TestHTTPWriteTimeoutHasHeadroomOverDNSExchangeTimeout(t *testing.T) {
	if httpWriteTimeout <= dnsExchangeTimeout {
		t.Fatalf("httpWriteTimeout (%s) must be greater than dnsExchangeTimeout (%s)", httpWriteTimeout, dnsExchangeTimeout)
	}
}

func TestDoHHandlerGetRejectsQueryOverMaxBody(t *testing.T) {
	const smallMaxBody = 16
	h := dohHandler("127.0.0.1:1", "/dns-query", smallMaxBody, 1)

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
	h := dohHandler("127.0.0.1:1", "/dns-query", maxDNSPacket, 1)

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
	h := dohHandler(addr, "/dns-query", maxDNSPacket, 1)

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

func TestReadyEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	h := dohHandler(ln.Addr().String(), "/dns-query", maxDNSPacket, 1)
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

	h := dohHandler(addr, "/dns-query", maxDNSPacket, 1)
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
