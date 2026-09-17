package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	tcp, err := net.Listen("tcp", "127.0.0.1:"+itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()

	want := []byte{0, 1, 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}

	go func() {
		buf := make([]byte, maxDNSPacket)
		n, addr, err := udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		truncated := append([]byte(nil), want...)
		truncated[2] |= 0x02
		_, _ = udp.WriteToUDP(truncated, addr)
		_ = n
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
	got, err := dnsExchange(context.Background(), "127.0.0.1:"+itoa(port), query)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("dnsExchange() = %x, want %x", got, want)
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 6)
	for n > 0 {
		buf = append(buf, digits[n%10])
		n /= 10
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

func TestDoHHandlerGetAndPost(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	addr := "127.0.0.1:" + itoa(udp.LocalAddr().(*net.UDPAddr).Port)

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
