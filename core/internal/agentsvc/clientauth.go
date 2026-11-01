package agentsvc

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
)

// minTokenBytes: a shorter token disables its client (docs/adr/0066 §3).
const minTokenBytes = 32

// ClientInterceptor is the :9091 listener's (docs/adr/0066 §3): the principal
// is the client whose token the bearer matches. clients is the registry's
// current sync; a client is enabled when its tokenRef names an env var
// holding one or two comma-separated tokens of at least 32 bytes. The bearer
// is compared in constant time against every enabled token, with no early
// exit. No cookie is read here.
func ClientInterceptor(clients func() []definitions.Client, getenv func(string) string) connect.Interceptor {
	return &clientAuth{clients: clients, getenv: getenv}
}

type clientAuth struct {
	clients func() []definitions.Client
	getenv  func(string) string
	warned  sync.Map // client name -> reason, logged once
}

var errBadToken = errors.New("unknown client token")

// match returns the client the bearer names, or "".
func (c *clientAuth) match(h http.Header) string {
	bearer, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if !ok || bearer == "" {
		return ""
	}
	matched := ""
	for _, cl := range c.clients() {
		if cl.Spec.TokenRef == "" {
			continue // in-process only
		}
		for _, tok := range c.tokens(cl) {
			if subtle.ConstantTimeCompare([]byte(bearer), []byte(tok)) == 1 && matched == "" {
				matched = cl.Name
			}
		}
	}
	return matched
}

func (c *clientAuth) tokens(cl definitions.Client) []string {
	var out []string
	for _, t := range strings.Split(c.getenv(cl.Spec.TokenRef), ",") {
		if t = strings.TrimSpace(t); len(t) >= minTokenBytes {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		if _, seen := c.warned.LoadOrStore(cl.Name, true); !seen {
			slog.Warn("agentsvc: client disabled: its token is unset or shorter than 32 bytes", "client", cl.Name, "tokenRef", cl.Spec.TokenRef)
		}
	}
	return out
}

func (c *clientAuth) principal(ctx context.Context, procedure string, h http.Header) (context.Context, error) {
	name := c.match(h)
	if name == "" {
		return nil, errkeys.Connect(connect.CodeUnauthenticated, errkeys.KeyInternal, errBadToken)
	}
	p := Principal{Client: name}
	if !allowed(procedure, p) {
		slog.WarnContext(ctx, "agentsvc: refused client call", "client", name, "procedure", procedure)
		return nil, errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, errors.New("not available to clients"))
	}
	return withPrincipal(ctx, p), nil
}

func (c *clientAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := c.principal(ctx, req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (c *clientAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (c *clientAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := c.principal(ctx, conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
