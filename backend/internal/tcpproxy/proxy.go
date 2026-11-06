// Package tcpproxy creates TCP connections through an explicit proxy.
// It does not read environment proxies or fall back to a direct connection.
package tcpproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

var ErrInvalidURL = errors.New("invalid TCP proxy URL; expected HTTP, HTTPS, SOCKS5 or SOCKS5H")

func parse(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, ErrInvalidURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, ErrInvalidURL
	}
	if u.Hostname() == "" || strings.ContainsAny(u.Hostname(), " \t\r\n/\\") ||
		u.Opaque != "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasSuffix(u.Host, ":") {
		return nil, ErrInvalidURL
	}
	if strings.Contains(u.Hostname(), ":") && !strings.HasPrefix(u.Host, "[") {
		return nil, ErrInvalidURL
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, ErrInvalidURL
		}
	}
	if u.User != nil && (u.Scheme == "socks5" || u.Scheme == "socks5h") {
		password, _ := u.User.Password()
		if len(u.User.Username()) == 0 || len(u.User.Username()) > 255 || len(password) > 255 {
			return nil, ErrInvalidURL
		}
	}
	return u, nil
}

// Normalize validates a URL without including its credentials in errors.
func Normalize(raw string) (string, error) {
	u, err := parse(raw)
	if err != nil || u == nil {
		return "", err
	}
	return u.String(), nil
}

type Dialer struct {
	proxy     *url.URL
	address   string
	direct    net.Dialer
	socks     xproxy.ContextDialer
	tlsConfig *tls.Config
}

func New(raw string) (*Dialer, error) {
	u, err := parse(raw)
	if err != nil {
		return nil, err
	}
	d := &Dialer{proxy: u, direct: net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}}
	if u == nil {
		return d, nil
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			port = "1080"
		}
	}
	d.address = net.JoinHostPort(u.Hostname(), port)
	if u.Scheme == "https" {
		d.tlsConfig = &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	}
	if u.Scheme == "socks5" || u.Scheme == "socks5h" {
		var auth *xproxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &xproxy.Auth{User: u.User.Username(), Password: password}
		}
		socks, err := xproxy.SOCKS5("tcp", d.address, auth, &d.direct)
		if err != nil {
			return nil, ErrInvalidURL
		}
		d.socks = socks.(xproxy.ContextDialer)
	}
	return d, nil
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d.proxy == nil {
		return d.direct.DialContext(ctx, network, address)
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("proxy requires a TCP connection")
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if d.socks != nil {
		target := address
		if d.proxy.Scheme == "socks5" {
			var err error
			target, err = resolveTarget(handshakeCtx, address)
			if err != nil {
				return nil, dialError(handshakeCtx)
			}
		}
		conn, err := d.socks.DialContext(handshakeCtx, network, target)
		if err != nil {
			return nil, dialError(handshakeCtx)
		}
		return conn, nil
	}
	return d.connectHTTP(handshakeCtx, address)
}

func resolveTarget(ctx context.Context, address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil {
		return address, err
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	if len(addresses) == 0 {
		return "", errors.New("proxy target has no IP addresses")
	}
	return net.JoinHostPort(addresses[0].IP.String(), port), nil
}

func dialError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("proxy connection or handshake failed")
}

func (d *Dialer) connectHTTP(ctx context.Context, address string) (_ net.Conn, resultErr error) {
	raw, err := d.direct.DialContext(ctx, "tcp", d.address)
	if err != nil {
		return nil, dialError(ctx)
	}
	// Cancellation must also interrupt TLS handshakes and CONNECT responses.
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer func() {
		stop()
		if resultErr != nil {
			_ = raw.Close()
		}
	}()
	conn := raw
	if d.proxy.Scheme == "https" {
		secure := tls.Client(raw, d.tlsConfig.Clone())
		if err := secure.HandshakeContext(ctx); err != nil {
			return nil, dialError(ctx)
		}
		conn = secure
	}
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if d.proxy.User != nil {
		password, _ := d.proxy.User.Password()
		credentials := d.proxy.User.Username() + ":" + password
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
	}
	if err := request.Write(conn); err != nil {
		return nil, dialError(ctx)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, dialError(ctx)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy CONNECT failed (%d)", response.StatusCode)
	}
	// The reader may already contain tunnel bytes after the CONNECT headers.
	// Do not close response.Body: it belongs to the established connection.
	if !stop() || ctx.Err() != nil {
		return nil, dialError(ctx)
	}
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
