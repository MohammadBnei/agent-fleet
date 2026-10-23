// Package bridgeclient is core's client of the event bridge (infra-bootstrap
// ADR-0060/0066, docs/adr/0078 §1). Core signs in as the authentik service
// account bridge-core with a client_credentials grant; its app password is an
// optional mounted file, re-read before every call, so core starts and runs
// whether or not the bridge is configured.
package bridgeclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
	"github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1/bridgev1connect"
)

// ErrNotConfigured: no bridge URL, or no usable app password file. Callers
// skip the bridge quietly.
var ErrNotConfigured = errors.New("event bridge not configured")

// Config is core's env (BRIDGE_*). Only the password is secret, and it is a
// file path, never a value.
type Config struct {
	URL          string // the bridge's in-cluster API, e.g. http://ukubi-bridge.ukubi-bridge.svc.cluster.local:8080
	TokenURL     string // authentik's token endpoint, reached through its in-cluster Service
	AuthHost     string // authentik's public host: the token's issuer is built from it
	ClientID     string // the ukubi-bridge-api provider
	Username     string // bridge-core
	PasswordFile string // the mounted app password
}

type Client struct {
	cfg  Config
	http *http.Client

	mu  sync.Mutex
	pw  string
	src oauth2.TokenSource
	api bridgev1connect.BridgeServiceClient
}

func New(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
}

// password reads the mounted file. An Infisical key that does not exist
// renders as the literal "<no value>" (infra-bootstrap ADR-0049): unset.
func (c *Client) password() (string, error) {
	if c.cfg.URL == "" || c.cfg.PasswordFile == "" {
		return "", ErrNotConfigured
	}
	b, err := os.ReadFile(c.cfg.PasswordFile)
	if err != nil {
		return "", ErrNotConfigured
	}
	pw := strings.TrimSpace(string(b))
	if pw == "" || pw == "<no value>" {
		return "", ErrNotConfigured
	}
	return pw, nil
}

// hostRewrite sends the token request to authentik's in-cluster Service as
// its public host, so the issuer matches what the bridge pins and nothing
// depends on Cloudflare (infra-bootstrap ADR-0066 §1).
type hostRewrite struct {
	host string
	base http.RoundTripper
}

func (h hostRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if h.host != "" {
		r.Host = h.host
		r.Header.Set("X-Forwarded-Proto", "https")
	}
	return h.base.RoundTrip(r)
}

// API returns the bridge client, signing in again when the password changed.
func (c *Client) API() (bridgev1connect.BridgeServiceClient, error) {
	pw, err := c.password()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.api != nil && pw == c.pw {
		return c.api, nil
	}
	cc := clientcredentials.Config{
		ClientID:       c.cfg.ClientID,
		TokenURL:       c.cfg.TokenURL,
		EndpointParams: url.Values{"username": {c.cfg.Username}, "password": {pw}},
		AuthStyle:      oauth2.AuthStyleInParams,
	}
	tokenHTTP := &http.Client{Timeout: 15 * time.Second, Transport: hostRewrite{host: c.cfg.AuthHost, base: http.DefaultTransport}}
	// The token source reuses a token until 10 s before it expires
	// (oauth2's expiry delta), above the bridge's 5 s floor for a stream.
	c.src = cc.TokenSource(context.WithValue(context.Background(), oauth2.HTTPClient, tokenHTTP))
	src := c.src
	bearer := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			t, err := src.Token()
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}
			req.Header().Set("Authorization", "Bearer "+t.AccessToken)
			return next(ctx, req)
		}
	})
	c.pw = pw
	c.api = bridgev1connect.NewBridgeServiceClient(c.http, c.cfg.URL, connect.WithInterceptors(bearer))
	return c.api, nil
}

// Publish appends one event as core. A duplicate (same source and id) is not
// an error: the bridge already has it.
func (c *Client) Publish(ctx context.Context, e *bridgev1.CloudEvent) error {
	api, err := c.API()
	if err != nil {
		return err
	}
	_, err = api.Publish(ctx, connect.NewRequest(&bridgev1.PublishRequest{Event: e}))
	return err
}
