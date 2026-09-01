package outbox

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/stretchr/testify/require"
)

func TestConfiguredTransportsNeverAcknowledgeStubDelivery(t *testing.T) {
	msg := Message{ID: 1, Topic: TopicBookingConfirmed, Payload: []byte(`{}`), Attempts: 1}
	ctx := context.Background()
	log := slog.Default()

	webpush := NewWebPushNotifier(WebPushConfig{
		PublicKey: "public", PrivateKey: "private", Subject: "mailto:ops@example.test",
	}, log)
	err := webpush.Notify(ctx, msg)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrTransportUnavailable))

	email := NewEmailNotifier("noreply@example.test", log)
	err = email.Notify(ctx, msg)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrTransportUnavailable))
}

func TestNotifierForRejectsUnavailableConfiguredTransports(t *testing.T) {
	for _, kind := range []string{"webpush", "email"} {
		cfg := &config.Config{NotifierKind: kind, VAPIDPublicKey: "public", VAPIDPrivateKey: "private", VAPIDSubject: "mailto:ops@example.test", EmailFrom: "noreply@example.test"}
		_, err := NotifierFor(cfg, nil)
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrTransportUnavailable))
	}
}

func TestUnconfiguredTransportsReturnConfigurationError(t *testing.T) {
	msg := Message{ID: 1, Topic: TopicBookingConfirmed, Payload: []byte(`{}`), Attempts: 1}
	err := NewWebPushNotifier(WebPushConfig{}, nil).Notify(context.Background(), msg)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrTransportNotConfigured))
	err = NewEmailNotifier("", nil).Notify(context.Background(), msg)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrTransportNotConfigured))
}
