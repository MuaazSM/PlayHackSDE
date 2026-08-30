package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// Message is one drained side effect, handed to a Notifier after its claim has
// committed.
type Message struct {
	// ID is the outbox row. Stable across retries — a transport that wants to
	// deduplicate has a key to do it with.
	ID int64

	// Topic is one of the Topic* constants.
	Topic string

	// Payload is the raw jsonb, undecoded. The dispatcher has no opinion about
	// what a topic's body means; transports and tests decode what they need.
	Payload json.RawMessage

	// Attempts is how many times this row has been claimed, including this one.
	// 1 on the first delivery.
	Attempts int
}

// Decode unmarshals the payload into v.
func (m Message) Decode(v any) error {
	if err := json.Unmarshal(m.Payload, v); err != nil {
		return fmt.Errorf("outbox: decode %s payload: %w", m.Topic, err)
	}
	return nil
}

// Notifier is the one transport interface. Web Push, email and the demo logger
// all sit behind it, so the dispatcher never learns how a notification travels.
//
// Delivery is AT-LEAST-ONCE, deliberately. The alternative — marking a row sent
// only after the transport confirms — loses notifications whenever the process
// dies between the send and the acknowledgement, and a lost "you got the court"
// is worse than a duplicate one. Duplicates are safe because the client side is
// idempotent: a push notification is keyed by its tag and a second
// "booking confirmed" for the same booking replaces the first rather than
// stacking. Implementations should be safe to call twice with the same
// Message.ID.
//
// A returned error means "not delivered, retry later". Returning nil for a
// permanent failure (a dead subscription, a bounced address) is correct — there
// is nothing to retry — and implementations should log rather than error there.
type Notifier interface {
	Notify(ctx context.Context, msg Message) error
}

// ErrTransportNotConfigured means the transport has no credentials. Returned
// rather than silently dropping: a mis-deployed VAPID key should show up as
// retries and a loud log line, not as notifications that quietly never arrive.
var ErrTransportNotConfigured = errors.New("outbox: transport not configured")

// ---------------------------------------------------------------------------
// LogNotifier — the demo default.

// LogNotifier writes each side effect to the log and succeeds.
//
// This is what the demo runs with, and that is a deliberate choice rather than
// laziness: the race console and the SSE stream carry everything the audience
// actually sees, and a Web Push round trip over venue wifi is a network
// dependency that can break the stage (IMPLEMENTATION.md §8, PRD §11.3). The
// outbox mechanism is identical either way — only the last hop changes.
type LogNotifier struct {
	log *slog.Logger
}

// NewLogNotifier returns the demo transport. A nil logger falls back to the
// default one.
func NewLogNotifier(log *slog.Logger) *LogNotifier {
	if log == nil {
		log = slog.Default()
	}
	return &LogNotifier{log: log}
}

// Notify logs the message. It never fails, so the demo never shows a retry.
func (n *LogNotifier) Notify(ctx context.Context, msg Message) error {
	n.log.InfoContext(ctx, "notification",
		"outbox_id", msg.ID,
		"topic", msg.Topic,
		"attempt", msg.Attempts,
		"payload", string(msg.Payload))
	return nil
}

// ---------------------------------------------------------------------------
// WebPushNotifier — VAPID. Stubbed.

// WebPushConfig holds the VAPID application-server identity. The keypair is
// generated once and pinned; rotating it invalidates every existing browser
// subscription, so it belongs in configuration rather than being minted at boot.
type WebPushConfig struct {
	PublicKey  string
	PrivateKey string

	// Subject is the mailto: or https: URL push services contact if this
	// application server misbehaves. Required by RFC 8292.
	Subject string
}

// Configured reports whether a real send could be attempted.
func (c WebPushConfig) Configured() bool {
	return c.PublicKey != "" && c.PrivateKey != "" && c.Subject != ""
}

// WebPushNotifier delivers over the Web Push protocol.
//
// STUB. The signing and the POST to each subscription endpoint are not
// implemented — that is a browser-integration task, not a concurrency one, and
// this project is judged on the write path. What is real is the shape: the
// dispatcher hands it a committed Message and takes back an error meaning
// "retry", so swapping the body of Notify for a real webpush client changes
// nothing above it.
//
// Subscriptions would be looked up per recipient from a push_subscriptions
// table; there is no such table yet and this phase does not add one.
type WebPushNotifier struct {
	cfg WebPushConfig
	log *slog.Logger
}

// NewWebPushNotifier builds the Web Push transport.
func NewWebPushNotifier(cfg WebPushConfig, log *slog.Logger) *WebPushNotifier {
	if log == nil {
		log = slog.Default()
	}
	return &WebPushNotifier{cfg: cfg, log: log}
}

// Notify would sign a VAPID JWT and POST the payload to each of the recipient's
// subscription endpoints.
func (n *WebPushNotifier) Notify(ctx context.Context, msg Message) error {
	if !n.cfg.Configured() {
		return fmt.Errorf("%w: web push (VAPID keypair and subject required)", ErrTransportNotConfigured)
	}

	n.log.InfoContext(ctx, "web push (stub)",
		"outbox_id", msg.ID,
		"topic", msg.Topic,
		"attempt", msg.Attempts,
		"subject", n.cfg.Subject)
	return nil
}

// ---------------------------------------------------------------------------
// EmailNotifier — fallback. Stubbed.

// EmailNotifier is the fallback for students with no push subscription.
//
// STUB, for the same reason as WebPushNotifier: an SMTP client proves nothing
// about the property this system is built to demonstrate. From is required so a
// misconfiguration fails loudly at the first send rather than producing mail
// nobody can reply to.
type EmailNotifier struct {
	from string
	log  *slog.Logger
}

// NewEmailNotifier builds the email transport. from is the envelope sender.
func NewEmailNotifier(from string, log *slog.Logger) *EmailNotifier {
	if log == nil {
		log = slog.Default()
	}
	return &EmailNotifier{from: from, log: log}
}

// Notify would render the topic's template and hand it to the campus relay.
func (n *EmailNotifier) Notify(ctx context.Context, msg Message) error {
	if n.from == "" {
		return fmt.Errorf("%w: email (sender address required)", ErrTransportNotConfigured)
	}

	n.log.InfoContext(ctx, "email (stub)",
		"outbox_id", msg.ID,
		"topic", msg.Topic,
		"attempt", msg.Attempts,
		"from", n.from)
	return nil
}
