package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// captureClientHello drives a real TLS client against a pipe and returns the
// exact ClientHello bytes the relay would see.
func captureClientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	clientConn, relayConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = relayConn.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	tlsClient := tls.Client(clientConn, &tls.Config{ServerName: serverName, InsecureSkipVerify: true})
	go func() { _ = tlsClient.HandshakeContext(ctx) }()

	prefix, sni, err := ReadClientHello(relayConn, 8192, 5*time.Second)
	if err != nil {
		t.Fatalf("ReadClientHello() error = %v", err)
	}
	if sni != serverName {
		t.Fatalf("ReadClientHello() sni = %q, want %q", sni, serverName)
	}
	return prefix
}

// TestClientHelloSNIFromRealTLSClient is the whole relay contract in one test:
// peek a real ClientHello, then replay those exact bytes into a TLS origin and
// complete the handshake and a request over the relayed connection.
func TestClientHelloSNIFromRealTLSClient(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "origin-body")
	}))
	t.Cleanup(origin.Close)

	relayListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = relayListener.Close() })

	clientConn, err := net.Dial("tcp", relayListener.Addr().String())
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	tlsClient := tls.Client(clientConn, &tls.Config{ServerName: "api.solian.app", InsecureSkipVerify: true})
	handshakeResult := make(chan error, 1)
	go func() { handshakeResult <- tlsClient.HandshakeContext(context.Background()) }()

	accepted, err := relayListener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { _ = accepted.Close() })

	prefix, sni, err := ReadClientHello(accepted, 8192, 5*time.Second)
	if err != nil {
		t.Fatalf("ReadClientHello() error = %v", err)
	}
	if sni != "api.solian.app" {
		t.Fatalf("ReadClientHello() sni = %q, want api.solian.app", sni)
	}

	upstream, err := net.Dial("tcp", origin.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial origin: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	if _, err := upstream.Write(prefix); err != nil {
		t.Fatalf("replay client hello: %v", err)
	}
	go func() { _, _ = io.Copy(upstream, accepted) }()
	go func() { _, _ = io.Copy(accepted, upstream) }()

	select {
	case err := <-handshakeResult:
		if err != nil {
			t.Fatalf("handshake after the relay replayed the client hello: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake did not finish after the client hello was replayed")
	}

	request, err := http.NewRequest("GET", "https://api.solian.app/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := request.Write(tlsClient); err != nil {
		t.Fatalf("write request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(tlsClient), request)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "origin-body" {
		t.Fatalf("response = %d %q, want 200 origin-body", response.StatusCode, body)
	}
}

func TestClientHelloSNIFragmented(t *testing.T) {
	clientHello := captureClientHello(t, "fragmented.solian.app")

	serverConn, feedConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = feedConn.Close()
	})
	go func() {
		for _, b := range clientHello {
			if _, err := feedConn.Write([]byte{b}); err != nil {
				return
			}
		}
		_ = feedConn.Close()
	}()

	prefix, sni, err := ReadClientHello(serverConn, 8192, 5*time.Second)
	if err != nil {
		t.Fatalf("ReadClientHello() error = %v", err)
	}
	if sni != "fragmented.solian.app" {
		t.Fatalf("ReadClientHello() sni = %q, want fragmented.solian.app", sni)
	}
	if len(prefix) != len(clientHello) {
		t.Fatalf("ReadClientHello() read %d bytes, want the %d client hello bytes", len(prefix), len(clientHello))
	}
}

func TestClientHelloSNIRejectsNonTLS(t *testing.T) {
	if sni, err := ClientHelloSNI([]byte("GET / HTTP/1.1\r\nHost: api.solian.app\r\n\r\n")); !errors.Is(err, ErrNotTLS) {
		t.Fatalf("ClientHelloSNI(plaintext) error = %v, sni = %q; want ErrNotTLS", err, sni)
	}

	clientHello := captureClientHello(t, "api.solian.app")

	// A prefix of a valid hello is "need more bytes", never a wrong answer.
	if sni, err := ClientHelloSNI(clientHello[:len(clientHello)-8]); !errors.Is(err, errNeedMore) {
		t.Fatalf("ClientHelloSNI(truncated) error = %v, sni = %q; want errNeedMore", err, sni)
	}

	// A client hello body length that cannot hold the fields it claims is
	// malformed (offset 5 is the handshake type, 6..8 the body length).
	lying := make([]byte, 0, len(clientHello))
	lying = append(lying, clientHello...)
	lying[6], lying[7], lying[8] = 0, 0, 4
	if sni, err := ClientHelloSNI(lying); !errors.Is(err, ErrMalformed) {
		t.Fatalf("ClientHelloSNI(lying length) error = %v, sni = %q; want ErrMalformed", err, sni)
	}

	// A handshake message that is not a ClientHello is rejected.
	notClientHello := make([]byte, len(clientHello))
	copy(notClientHello, clientHello)
	notClientHello[5] = 0x02
	if sni, err := ClientHelloSNI(notClientHello); !errors.Is(err, ErrMalformed) {
		t.Fatalf("ClientHelloSNI(server hello) error = %v, sni = %q; want ErrMalformed", err, sni)
	}
}

func TestReadClientHelloRequiresSNI(t *testing.T) {
	clientConn, relayConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = relayConn.Close()
	})
	// ECH and non-TLS clients have no cleartext SNI: the relay must reject them
	// rather than guess a route.
	go func() {
		_, _ = clientConn.Write([]byte("hello"))
	}()

	if _, _, err := ReadClientHello(relayConn, 64, 2*time.Second); !errors.Is(err, ErrNotTLS) {
		t.Fatalf("ReadClientHello(plaintext) error = %v, want ErrNotTLS", err)
	}
}

func TestReadClientHelloHonorsMaxBytes(t *testing.T) {
	clientConn, relayConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = relayConn.Close()
	})
	go func() {
		// A handshake record header claiming far more bytes than maxBytes.
		_, _ = clientConn.Write([]byte{recordTypeHandshake, 0x03, 0x01, 0xff, 0xff})
	}()

	if _, _, err := ReadClientHello(relayConn, 64, 500*time.Millisecond); !errors.Is(err, ErrMalformed) {
		t.Fatalf("ReadClientHello(oversized) error = %v, want ErrMalformed", err)
	}
}
