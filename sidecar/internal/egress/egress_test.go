package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// echo listens and echoes one line per connection, prefixed with name.
func echo(t *testing.T, name string) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				line, _ := bufio.NewReader(c).ReadString('\n')
				_, _ = io.WriteString(c, name+":"+line)
			}()
		}
	}()
	return l
}

// fakeVault is a CONNECT proxy that records the auth it got and answers
// status, then echoes like echo("vault").
type fakeVault struct {
	net.Listener
	mu     sync.Mutex
	auth   []string
	status string
}

func newFakeVault(t *testing.T, status string) *fakeVault {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	v := &fakeVault{Listener: l, status: status}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				v.mu.Lock()
				v.auth = append(v.auth, req.Method+" "+req.Host+" "+req.Header.Get("Proxy-Authorization"))
				v.mu.Unlock()
				_, _ = io.WriteString(c, "HTTP/1.1 "+v.status+"\r\n\r\n")
				if !strings.HasPrefix(v.status, "200") {
					return
				}
				line, _ := br.ReadString('\n')
				_, _ = io.WriteString(c, "vault:"+line)
			}()
		}
	}()
	return v
}

// setup returns a router listening on loopback whose dialer sends every
// target except the vault to direct.
func setup(t *testing.T, vaultStatus string) (proxy string, v *fakeVault, dialed *[]string) {
	t.Helper()
	v = newFakeVault(t, vaultStatus)
	direct := echo(t, "direct")
	r, err := New("http://tok:owner-o@"+v.Addr().String(), "github.com, API.github.com")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	dialed = &[]string{}
	r.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		*dialed = append(*dialed, addr)
		mu.Unlock()
		if addr != v.Addr().String() {
			addr = direct.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), v, dialed
}

// tunnel CONNECTs to target through proxy, sends one line, returns the reply
// or the CONNECT status line.
func tunnel(t *testing.T, proxy, target string) string {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.Status
	}
	_, _ = io.WriteString(c, "hi\n")
	line, _ := br.ReadString('\n')
	return strings.TrimSpace(line)
}

func TestRouter_VaultHostsGoThroughTheVaultWithTheToken(t *testing.T) {
	proxy, v, _ := setup(t, "200 Connection Established")
	for _, target := range []string{"github.com:443", "api.github.com:443", "GitHub.com.:443"} {
		if got := tunnel(t, proxy, target); got != "vault:hi" {
			t.Errorf("%s: %q, want through the vault", target, got)
		}
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("tok:owner-o"))
	for _, a := range v.auth {
		if !strings.HasPrefix(a, "CONNECT ") || !strings.HasSuffix(a, " "+want) {
			t.Errorf("vault got %q", a)
		}
	}
}

// Everything else is tunnelled direct: HTTP/2 and gRPC pass untouched, and
// the vault never sees it. So is a vault host on another port: its services
// are pinned to :443.
func TestRouter_OtherHostsGoDirect(t *testing.T) {
	proxy, v, dialed := setup(t, "200 Connection Established")
	for _, target := range []string{"registry.npmjs.org:443", "grpcb.in:9001", "github.com:22", "evilgithub.com:443"} {
		if got := tunnel(t, proxy, target); got != "direct:hi" {
			t.Errorf("%s: %q, want direct", target, got)
		}
	}
	if len(v.auth) != 0 {
		t.Errorf("vault saw %v", v.auth)
	}
	if !strings.Contains(strings.Join(*dialed, ","), "grpcb.in:9001") {
		t.Errorf("dialed %v", *dialed)
	}
}

func TestRouter_VaultRefusalIsABadGateway(t *testing.T) {
	proxy, _, _ := setup(t, "407 Proxy Authentication Required")
	if got := tunnel(t, proxy, "github.com:443"); !strings.HasPrefix(got, "502") {
		t.Errorf("got %q, want 502", got)
	}
}

func TestRouter_ConnectOnly(t *testing.T) {
	proxy, _, _ := setup(t, "200 Connection Established")
	resp, err := http.Get("http://" + proxy + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status %d", resp.StatusCode)
	}
}

// The token never leaves through an error: New does not quote a bad URL.
func TestNew_RefusesAProxyWithoutAToken(t *testing.T) {
	for _, raw := range []string{"", "http://host:1", "::"} {
		if _, err := New(raw, "github.com"); err == nil {
			t.Errorf("New(%q) accepted", raw)
		}
	}
	if _, err := url.Parse("http://tok:v@h:1"); err != nil {
		t.Fatal(err)
	}
}
