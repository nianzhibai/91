package tcpproxy

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProxyURLValidation(t *testing.T) {
	for _, raw := range []string{"", "  ", "http://localhost", "https://user:p%40ss@proxy.example:443/", "socks5://127.0.0.1:1080", "socks5h://[::1]:1080"} {
		if _, err := New(raw); err != nil {
			t.Errorf("rejected valid URL: %v", err)
		}
	}
	for _, raw := range []string{
		"localhost:1080", "ftp://user:PRIVATE_PASSWORD@host:21", "http://", "http://:8080", "http://localhost:",
		"http://host:0", "socks5://host:65536", "https://host:abc", "http://host/path", "http://host?x=1", "http://host#fragment",
		"socks5://::1:1080",
		"socks5://:PRIVATE_PASSWORD@host:1080", "socks5://user:" + strings.Repeat("a", 256) + "@host:1080",
	} {
		if _, err := New(raw); err == nil || strings.Contains(err.Error(), "PRIVATE_PASSWORD") {
			t.Fatalf("invalid URL accepted or credentials exposed: %v", err)
		}
	}
	normalized, err := Normalize("  SOCKS5H://user:p%40ss@localhost:1080  ")
	if err != nil || normalized != "socks5h://user:p%40ss@localhost:1080" {
		t.Fatalf("normalization: %s %v", normalized, err)
	}
	for _, tc := range []struct{ raw, port string }{{"http://host", "80"}, {"https://host", "443"}, {"socks5://host", "1080"}} {
		dialer, err := New(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if dialer.address != "host:"+tc.port {
			t.Fatalf("default port: %s", dialer.address)
		}
	}
}

func httpProxy(t *testing.T, secure bool, status int) (*httptest.Server, <-chan *http.Request) {
	t.Helper()
	requests := make(chan *http.Request, 4)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("unexpected proxy method %s", r.Method)
			w.WriteHeader(405)
			return
		}
		requests <- r
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if status != 200 {
			_, _ = fmt.Fprintf(rw, "HTTP/1.1 %d PRIVATE_PASSWORD\r\nContent-Length: 0\r\n\r\n", status)
			_ = rw.Flush()
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\nprefetched")
		if err := rw.Flush(); err != nil {
			return
		}
		_, _ = io.Copy(conn, rw)
	})
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	if secure {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server, requests
}

func assertTunnel(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	greeting := make([]byte, len("prefetched"))
	if _, err := io.ReadFull(conn, greeting); err != nil || string(greeting) != "prefetched" {
		t.Fatalf("lost prefetched tunnel data: %q %v", greeting, err)
	}
	if _, err := io.WriteString(conn, "payload"); err != nil {
		t.Fatal(err)
	}
	echoed := make([]byte, len("payload"))
	if _, err := io.ReadFull(conn, echoed); err != nil || string(echoed) != "payload" {
		t.Fatalf("tunnel read/write failed: %q %v", echoed, err)
	}
}

func TestHTTPConnectProxyAuthenticationAndBufferedData(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			server, requests := httpProxy(t, secure, 200)
			proxyURL, _ := url.Parse(server.URL)
			proxyURL.User = url.UserPassword("user", "p@ss")
			dialer, err := New(proxyURL.String())
			if err != nil {
				t.Fatal(err)
			}
			if secure {
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				dialer.tlsConfig.RootCAs = roots
			}
			ctx, cancel := context.WithCancel(context.Background())
			conn, err := dialer.DialContext(ctx, "tcp", "203.0.113.7:443")
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			assertTunnel(t, conn) // Canceling a completed dial must not close the tunnel.
			request := <-requests
			if request.Host != "203.0.113.7:443" || request.RequestURI != request.Host {
				t.Fatalf("wrong CONNECT destination: %s %s", request.Host, request.RequestURI)
			}
			if got := request.Header.Get("Proxy-Authorization"); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("user:p@ss")) {
				t.Fatal("proxy authentication missing")
			}
		})
	}
}

func TestHTTPSProxyVerifiesCertificates(t *testing.T) {
	server, _ := httpProxy(t, true, 200)
	dialer, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "203.0.113.7:443")
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("untrusted HTTPS proxy certificate accepted")
	}
}

func TestHTTPProxyFailureDoesNotFallBackOrExposeCredentials(t *testing.T) {
	server, _ := httpProxy(t, false, 407)
	proxyURL, _ := url.Parse(server.URL)
	proxyURL.User = url.UserPassword("user", "PRIVATE_PASSWORD")
	dialer, _ := New(proxyURL.String())
	conn, err := dialer.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if conn != nil {
		conn.Close()
		t.Fatal("failed proxy returned a connection")
	}
	if err == nil || !strings.Contains(err.Error(), "407") || strings.Contains(err.Error(), "PRIVATE_PASSWORD") {
		t.Fatalf("unsafe or missing proxy error: %v", err)
	}
}

type socksRequest struct {
	host               string
	port               uint16
	username, password string
}

func socksProxy(t *testing.T, auth bool, reject bool) (string, <-chan socksRequest) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	requests := make(chan socksRequest, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(conn)
		greeting := make([]byte, 2)
		if _, err := io.ReadFull(reader, greeting); err != nil {
			return
		}
		if greeting[0] != 5 {
			t.Error("SOCKS version mismatch")
			return
		}
		methods := make([]byte, int(greeting[1]))
		if _, err := io.ReadFull(reader, methods); err != nil {
			return
		}
		method := byte(0)
		if auth {
			method = 2
		}
		if _, err := conn.Write([]byte{5, method}); err != nil {
			return
		}
		var request socksRequest
		if auth {
			header := make([]byte, 2)
			if _, err := io.ReadFull(reader, header); err != nil {
				return
			}
			username := make([]byte, int(header[1]))
			if _, err := io.ReadFull(reader, username); err != nil {
				return
			}
			n, err := reader.ReadByte()
			if err != nil {
				return
			}
			password := make([]byte, int(n))
			if _, err := io.ReadFull(reader, password); err != nil {
				return
			}
			request.username, request.password = string(username), string(password)
			status := byte(0)
			if reject {
				status = 1
			}
			if _, err := conn.Write([]byte{1, status}); err != nil {
				return
			}
			if reject {
				requests <- request
				return
			}
		}
		header := make([]byte, 4)
		if _, err := io.ReadFull(reader, header); err != nil {
			return
		}
		if header[0] != 5 || header[1] != 1 {
			t.Error("expected SOCKS CONNECT")
			return
		}
		switch header[3] {
		case 1:
			ip := make([]byte, 4)
			if _, err := io.ReadFull(reader, ip); err != nil {
				return
			}
			request.host = net.IP(ip).String()
		case 4:
			ip := make([]byte, 16)
			if _, err := io.ReadFull(reader, ip); err != nil {
				return
			}
			request.host = net.IP(ip).String()
		case 3:
			n, err := reader.ReadByte()
			if err != nil {
				return
			}
			host := make([]byte, int(n))
			if _, err := io.ReadFull(reader, host); err != nil {
				return
			}
			request.host = string(host)
		default:
			t.Error("unexpected SOCKS address type")
			return
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(reader, port); err != nil {
			return
		}
		request.port = binary.BigEndian.Uint16(port)
		requests <- request
		if _, err := conn.Write(append([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}, []byte("prefetched")...)); err != nil {
			return
		}
		_, _ = io.Copy(conn, reader)
	}()
	return listener.Addr().String(), requests
}

func TestSOCKSProxyDNSAuthenticationAndTunnel(t *testing.T) {
	for _, tc := range []struct {
		scheme, target string
		auth           bool
	}{
		{"socks5", "127.0.0.1:443", false},
		{"socks5", "localhost:443", true},
		{"socks5h", "telegram-test.invalid:443", true},
	} {
		t.Run(tc.scheme+tc.target, func(t *testing.T) {
			address, requests := socksProxy(t, tc.auth, false)
			proxyURL := &url.URL{Scheme: tc.scheme, Host: address}
			if tc.auth {
				proxyURL.User = url.UserPassword("user", "p@ss")
			}
			dialer, err := New(proxyURL.String())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			conn, err := dialer.DialContext(ctx, "tcp", tc.target)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			assertTunnel(t, conn)
			request := <-requests
			if request.port != 443 {
				t.Fatal("SOCKS target port changed")
			}
			if tc.scheme == "socks5h" && request.host != "telegram-test.invalid" {
				t.Fatal("SOCKS5H resolved the target locally")
			}
			if tc.scheme == "socks5" && net.ParseIP(request.host) == nil {
				t.Fatal("SOCKS5 did not resolve the target locally")
			}
			if tc.auth && (request.username != "user" || request.password != "p@ss") {
				t.Fatal("SOCKS authentication changed")
			}
		})
	}
}

func TestSOCKSAuthenticationFailureIsSanitized(t *testing.T) {
	address, requests := socksProxy(t, true, true)
	proxyURL := &url.URL{Scheme: "socks5h", Host: address, User: url.UserPassword("user", "PRIVATE_PASSWORD")}
	dialer, _ := New(proxyURL.String())
	conn, err := dialer.DialContext(context.Background(), "tcp", "telegram-test.invalid:443")
	if conn != nil {
		conn.Close()
	}
	if err == nil || strings.Contains(err.Error(), "PRIVATE_PASSWORD") {
		t.Fatalf("unsafe authentication error: %v", err)
	}
	if request := <-requests; request.password != "PRIVATE_PASSWORD" {
		t.Fatal("wrong credentials sent")
	}
}

func TestProxyHandshakeCancellationClosesConnection(t *testing.T) {
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted, closed := make(chan struct{}), make(chan struct{})
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				close(accepted)
				_, _ = io.Copy(io.Discard, conn)
				close(closed)
			}()
			dialer, _ := New(scheme + "://" + listener.Addr().String())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:443")
				if conn != nil {
					conn.Close()
				}
				result <- err
			}()
			select {
			case <-accepted:
			case <-time.After(2 * time.Second):
				t.Fatal("proxy not reached")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("proxy handshake did not stop")
			}
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("canceled tunnel leaked its connection")
			}
		})
	}
}

func TestEmptyProxyIgnoresEnvironment(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(key, "socks5://127.0.0.1:1")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "prefetched")
		_, _ = io.Copy(conn, conn)
	}()
	dialer, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	assertTunnel(t, conn)
}

func TestResolveTargetRetainsLiteralAddresses(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		address := net.JoinHostPort(host, strconv.Itoa(443))
		resolved, err := resolveTarget(context.Background(), address)
		if err != nil || resolved != address {
			t.Fatalf("literal address changed: %s %v", resolved, err)
		}
	}
}
