// Package sttclient is core's gRPC client for ukubi-stt's Recognize RPC —
// the speech-to-text service on the cluster's one GPU node (infra-bootstrap
// docs/adr/0044, 0046).
//
// Core is the only caller from this fleet, and deliberately so: the dashboard
// cannot reach ukubi-stt itself. Core allows no CORS and its session cookie is
// SameSite=Lax, so a browser call to another origin carries no identity — and
// putting an STT bearer token in the browser to work around that would hand
// every dashboard user a credential that authorises the GPU. Proxying here
// keeps the token server-side and lets core derive the session id from the
// authenticated user.
package sttclient

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	sttv1 "github.com/MohammadBnei/agent-fleet/core/proto/gen/go/stt/v1"
)

// sampleRateHertz is the only rate ukubi-stt accepts. Anything else is
// rejected with INVALID_ARGUMENT rather than resampled, because a silently
// resampled request returns a plausible transcript and a meaningless
// real-time factor — and the RTF is how a caller detects the service having
// fallen back to CPU.
const sampleRateHertz = 16000

// chunkCallTimeout bounds one ~560ms chunk. Measured in-cluster: 24-31ms round
// trip, ~23ms of it decode. Five seconds is far beyond anything healthy, which
// is the point — it exists to stop a wedged call holding a dashboard request
// open, not to express an expectation.
const chunkCallTimeout = 5 * time.Second

type Client struct {
	conn  *grpc.ClientConn
	rpc   sttv1.SttClient
	token string
}

// New dials addr (in-cluster ClusterIP, no TLS — same trust boundary as every
// other in-cluster call in this fleet).
func New(addr, token string) (*Client, error) {
	if token == "" {
		return nil, fmt.Errorf("stt: no token configured")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial stt: %w", err)
	}
	return &Client{conn: conn, rpc: sttv1.NewSttClient(conn), token: token}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// SessionID derives ukubi-stt's session id from the authenticated identity and
// a client-chosen stream id.
//
// HMAC rather than a plain hash, and derived here rather than passed through:
// ukubi-stt keys recognizer state on whatever id it is given, and core holds a
// single token on behalf of every dashboard user — so a browser-supplied id
// would let one user interleave audio into another user's dictation. The
// client half is deliberately per-DICTATION, not per-session: a fleet session
// is a conversation, an STT session is a recognizer lifecycle swept after 120s
// idle, and two tabs can dictate into one conversation.
//
// Keyed on the STT token, which is server-side only and required for the call
// to work at all — so there is no way to be half-configured.
func (c *Client) SessionID(identity, streamID string) string {
	mac := hmac.New(sha256.New, []byte(c.token))
	fmt.Fprintf(mac, "%s:%s", identity, streamID)
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// Transcribe sends one chunk of a dictation and returns the NEW text for it.
//
// last flushes the encoder's buffered tail and releases the recognizer. An
// empty chunk with last set is a valid bare close.
//
// NEVER retry a chunk. Order is load-bearing — the encoder carries cache
// forward, so a re-sent chunk arriving after a later one corrupts everything
// following it. On error the caller abandons the dictation.
func (c *Client) Transcribe(ctx context.Context, sessionID string, pcm []byte, last bool, language string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, chunkCallTimeout)
	defer cancel()

	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)

	resp, err := c.rpc.Recognize(ctx, &sttv1.RecognizeRequest{
		Config:    &sttv1.RecognitionConfig{SampleRateHertz: sampleRateHertz, Language: language},
		Audio:     pcm,
		SessionId: sessionID,
		Last:      last,
	})
	if err != nil {
		return "", err
	}
	return resp.GetText(), nil
}

// Unavailable reports whether err is one the caller should surface as "the
// service is busy or down" rather than as a bug.
//
// RESOURCE_EXHAUSTED means the cluster-wide session cap is full (there is one
// GPU); FAILED_PRECONDITION means the streaming model failed to load and the
// service will retry that load on the next request. Both are transient states
// of a single-replica service that ADR-0044 explicitly allows to be
// unavailable — neither is an internal error.
func Unavailable(err error) bool {
	switch status.Code(err) {
	case codes.ResourceExhausted, codes.FailedPrecondition, codes.Unavailable:
		return true
	default:
		return false
	}
}
