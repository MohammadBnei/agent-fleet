// Package egress is a repo pod's HTTPS_PROXY (docs/adr/0075 §7): a CONNECT
// router on loopback. The vault's hosts go on to Agent Vault, which injects
// the real credential; every other host is tunnelled direct, untouched.
//
// Agent Vault's MITM speaks HTTP/1.1 only and drops trailers, so sending
// everything through it broke HTTP/2 and gRPC to every external host, showed
// the vault all of the pod's HTTPS in plaintext, and spent its rate limit on
// package installs. It also kept the vault token in the worker's env, where
// Chromium ignored it (a proxy URL's userinfo) and the agent could read it.
// Here the token lives in the sidecar's env only.
package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Router routes CONNECT requests. Vault is the vault proxy URL with the pod's
// session token as its userinfo; Hosts are the hostnames it injects on (port
// 443 only: its services are pinned there).
type Router struct {
	Vault *url.URL
	Hosts map[string]bool
	Dial  func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New parses FLEET_VAULT_PROXY and FLEET_VAULT_HOSTS (comma-separated).
func New(vaultProxy, hosts string) (*Router, error) {
	u, err := url.Parse(vaultProxy)
	if err != nil || u.Host == "" || u.User == nil {
		return nil, errors.New("egress: FLEET_VAULT_PROXY is not a proxy URL with a token") // never quote it
	}
	r := &Router{Vault: u, Hosts: map[string]bool{}, Dial: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}
	for _, h := range strings.Split(hosts, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			r.Hosts[h] = true
		}
	}
	return r, nil
}

func (r *Router) toVault(target string) bool {
	host, port, err := net.SplitHostPort(target)
	return err == nil && port == "443" && r.Hosts[strings.ToLower(strings.TrimSuffix(host, "."))]
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodConnect {
		// HTTPS_PROXY only: plain http:// never comes here.
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	target := req.Host
	var up net.Conn
	var err error
	if r.toVault(target) {
		up, err = r.viaVault(req.Context(), target)
	} else {
		up, err = r.Dial(req.Context(), "tcp", target)
	}
	if err != nil {
		slog.Warn("egress: connect failed", "target", target, "error", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	client, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = up.Close()
		return
	}
	splice(client, buf.Reader, up) // buf reads the client conn, bytes it already buffered first
}

// viaVault opens a tunnel to target through the vault, authenticated as this
// pod's session.
func (r *Router) viaVault(ctx context.Context, target string) (net.Conn, error) {
	c, err := r.Dial(ctx, "tcp", r.Vault.Host)
	if err != nil {
		return nil, fmt.Errorf("vault proxy: %w", err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	pass, _ := r.Vault.User.Password()
	auth := base64.StdEncoding.EncodeToString([]byte(r.Vault.User.Username() + ":" + pass))
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target, target, auth); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("vault proxy: %w", err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("vault proxy: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = c.Close()
		return nil, fmt.Errorf("vault proxy refused %s: %s", target, resp.Status)
	}
	_ = c.SetDeadline(time.Time{})
	return &bufConn{Conn: c, r: br}, nil
}

// bufConn reads what the CONNECT response's reader already buffered first.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// splice copies both ways, half-closing each side as its source ends, and
// closes both once both directions are done.
func splice(client net.Conn, clientR io.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst net.Conn, src io.Reader) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(up, clientR)
	go cp(client, up)
	<-done
	<-done
	_ = client.Close()
	_ = up.Close()
}
