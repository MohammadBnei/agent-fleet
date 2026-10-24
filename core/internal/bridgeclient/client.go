// Package bridgeclient is core's client of the event bridge (infra-bootstrap
// ADR-0060/0066, docs/adr/0078 §1). Core signs in as the authentik service
// account bridge-core with a client_credentials grant; its app password is an
// optional mounted file, re-read before every call, so core starts and runs
// whether or not the bridge is configured.
package bridgeclient

import (
	"context"
	"errors"
	"fmt"
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
	cfg Config

	mu  sync.Mutex
	pw  string
	src oauth2.TokenSource
	api bridgev1connect.BridgeServiceClient
}

func New(cfg Config) *Client {
	return &Client{cfg: cfg}
}

// password reads the mounted file. An Infisical key that does not exist
// renders as the literal "<no value>" (infra-bootstrap ADR-0049): unset.
// Each refusal wraps ErrNotConfigured with its reason, which the caller logs
// once: a Secret delivered under the wrong name or key must not be silent.
func (c *Client) password() (string, error) {
	if c.cfg.URL == "" {
		return "", fmt.Errorf("%w: BRIDGE_URL is unset", ErrNotConfigured)
	}
	b, err := os.ReadFile(c.cfg.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("%w: app password file %s: %v", ErrNotConfigured, c.cfg.PasswordFile, err)
	}
	pw := strings.TrimSpace(string(b))
	if pw == "" || pw == "<no value>" {
		return "", fmt.Errorf("%w: app password file %s is empty or a missing Infisical key", ErrNotConfigured, c.cfg.PasswordFile)
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
	api, _, err := c.session()
	return api, err
}

// session is the client and its token source, rebuilt when the password
// changed.
func (c *Client) session() (bridgev1connect.BridgeServiceClient, oauth2.TokenSource, error) {
	pw, err := c.password()
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.api != nil && pw == c.pw {
		return c.api, c.src, nil
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
	src := cc.TokenSource(context.WithValue(context.Background(), oauth2.HTTPClient, tokenHTTP))
	bearer := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if err := c.authorize(src, req.Header()); err != nil {
				return nil, err
			}
			return next(ctx, req)
		}
	})
	c.pw, c.src = pw, src
	// The stream's client has no timeout: Subscribe is long-lived and ends
	// at the token's expiry or the caller's context.
	c.api = bridgev1connect.NewBridgeServiceClient(&http.Client{Transport: http.DefaultTransport}, c.cfg.URL, connect.WithInterceptors(bearer))
	return c.api, src, nil
}

func (c *Client) authorize(src oauth2.TokenSource, h http.Header) error {
	t, err := src.Token()
	if err != nil {
		return fmt.Errorf("authentik grant as %s: %w", c.cfg.Username, err)
	}
	h.Set("Authorization", "Bearer "+t.AccessToken)
	return nil
}

// settle drops the cached client when the bridge rejected its token (a
// rotated signing cert, a changed client table): the next call signs in
// afresh.
func (c *Client) settle(err error) error {
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		c.mu.Lock()
		c.api = nil
		c.mu.Unlock()
	}
	return err
}

// Publish appends one event as core. A duplicate (same source and id) is not
// an error: the bridge already has it.
func (c *Client) Publish(ctx context.Context, e *bridgev1.CloudEvent) error {
	api, err := c.API()
	if err != nil {
		return err
	}
	_, err = api.Publish(ctx, connect.NewRequest(&bridgev1.PublishRequest{Event: e}))
	return c.settle(err)
}

// Stream is one Subscribe call: Receive until it returns false, then Err.
type Stream interface {
	Receive() bool
	Msg() *bridgev1.SubscribeResponse
	Err() error
	Close() error
}

type stream struct {
	c *Client
	*connect.ServerStreamForClient[bridgev1.SubscribeResponse]
}

func (s stream) Err() error { return s.c.settle(s.ServerStreamForClient.Err()) }

// Subscribe opens core's subscription. The stream ends Unauthenticated when
// its token expires (the bridge's rule): reconnect.
func (c *Client) Subscribe(ctx context.Context, r *bridgev1.SubscribeRequest) (Stream, error) {
	api, src, err := c.session()
	if err != nil {
		return nil, err
	}
	req := connect.NewRequest(r)
	if err := c.authorize(src, req.Header()); err != nil {
		return nil, err
	}
	st, err := api.Subscribe(ctx, req)
	if err != nil {
		return nil, c.settle(err)
	}
	return stream{c, st}, nil
}

func (c *Client) Ack(ctx context.Context, subscription string, seq int64) error {
	api, err := c.API()
	if err != nil {
		return err
	}
	_, err = api.Ack(ctx, connect.NewRequest(&bridgev1.AckRequest{Subscription: subscription, Seq: seq}))
	return c.settle(err)
}

func (c *Client) Nack(ctx context.Context, subscription string, seq int64, reason string) error {
	api, err := c.API()
	if err != nil {
		return err
	}
	_, err = api.Nack(ctx, connect.NewRequest(&bridgev1.NackRequest{Subscription: subscription, Seq: seq, Reason: reason}))
	return c.settle(err)
}

func (c *Client) ConfirmGap(ctx context.Context, subscription string, version int64) error {
	api, err := c.API()
	if err != nil {
		return err
	}
	_, err = api.ConfirmGap(ctx, connect.NewRequest(&bridgev1.ConfirmGapRequest{Subscription: subscription, Version: version}))
	return c.settle(err)
}
