// Package egress is a sandbox's only way out: an HTTP proxy that reaches
// public addresses, and the server's job API by name (adr/0046).
package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

// Port is where the proxy listens; Host is its name on a sandbox's
// network.
const (
	Port = "3128"
	Host = "stackwright-egress"
)

// ErrRefused is a target that is not public and not allowed by name.
var ErrRefused = errors.New("the egress proxy refuses this address")

// reserved are blocks that netip does not mark as private but that are not
// public: shared address space, IETF protocol assignments, benchmarking,
// "this network", reserved, and NAT64, which can map to any of them.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2001:db8::/32"),
}

// Public reports whether an address is a public unicast one.
func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// Proxy forwards HTTP and tunnels CONNECT to public addresses, and to the
// host:port pairs in Allow whatever their address.
type Proxy struct {
	Allow map[string]bool
}

// dial resolves addr once, checks every address, and dials one it
// checked, so a name cannot resolve to another address between the check
// and the dial.
func (p *Proxy) dial(ctx context.Context, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	allowed := p.Allow[strings.ToLower(addr)]
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if !allowed && !Public(ip) {
			slog.Warn("egress refused", "target", addr, "address", ip.String())
			return nil, fmt.Errorf("%w: %s is %s", ErrRefused, addr, ip)
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	var last error
	for _, ip := range ips {
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Scheme != "http" {
		http.Error(w, "this is a proxy: ask for an absolute http:// URL, or CONNECT", http.StatusBadRequest)
		return
	}
	target := r.URL.Host
	if r.URL.Port() == "" {
		target += ":80"
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) { pr.Out.URL = r.URL; pr.Out.Host = r.Host },
		Transport: &http.Transport{
			DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return p.dial(ctx, target) },
			DisableKeepAlives: true,
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status := http.StatusBadGateway
			if errors.Is(err, ErrRefused) {
				status = http.StatusForbidden
			}
			http.Error(w, err.Error(), status)
		},
	}
	rp.ServeHTTP(w, r)
}

func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request) {
	upstream, err := p.dial(r.Context(), r.Host)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, ErrRefused) {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer func() { _ = upstream.Close() }()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot tunnel", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, buf); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// Serve runs the proxy on Port until ctx ends. allow lists host:port
// pairs, comma-separated.
func Serve(ctx context.Context, allow string) error {
	p := &Proxy{Allow: map[string]bool{}}
	for _, a := range strings.Split(allow, ",") {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			p.Allow[a] = true
		}
	}
	srv := &http.Server{Addr: ":" + Port, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	slog.Info("egress proxy listening", "port", Port, "allow", allow)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Env returns the proxy settings of this process, for a subprocess whose
// environment is built from nothing.
func Env() []string {
	var out []string
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "NO_PROXY", "no_proxy"} {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// Dial connects to addr through the proxy that HTTPS_PROXY names, with
// CONNECT, or directly when none is set. It is for a client that needs a
// raw stream to an http:// address, such as one that speaks HTTP/2
// without TLS.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return nil, err
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = c.Close()
		return nil, fmt.Errorf("the egress proxy refused %s: %s", addr, resp.Status)
	}
	if br.Buffered() > 0 {
		_ = c.Close()
		return nil, errors.New("the egress proxy sent data before the tunnel")
	}
	return c, nil
}
