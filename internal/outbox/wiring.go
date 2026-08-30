package outbox

import (
	"fmt"
	"log/slog"

	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/store"
)

// NotifierFor builds the transport named by cfg.NotifierKind.
//
// Lives here rather than in each cmd because both binaries need it and they must
// agree: an api running LogNotifier while a worker runs Web Push would deliver
// notifications differently depending on which process happened to drain the
// row.
func NotifierFor(cfg *config.Config, log *slog.Logger) (Notifier, error) {
	switch cfg.NotifierKind {
	case "", "log":
		return NewLogNotifier(log), nil

	case "webpush":
		wp := WebPushConfig{
			PublicKey:  cfg.VAPIDPublicKey,
			PrivateKey: cfg.VAPIDPrivateKey,
			Subject:    cfg.VAPIDSubject,
		}
		if !wp.Configured() {
			return nil, fmt.Errorf("%w: NOTIFIER=webpush needs VAPID_PUBLIC_KEY, VAPID_PRIVATE_KEY and VAPID_SUBJECT", ErrTransportNotConfigured)
		}
		return NewWebPushNotifier(wp, log), nil

	case "email":
		if cfg.EmailFrom == "" {
			return nil, fmt.Errorf("%w: NOTIFIER=email needs EMAIL_FROM", ErrTransportNotConfigured)
		}
		return NewEmailNotifier(cfg.EmailFrom, log), nil

	default:
		return nil, fmt.Errorf("outbox: unknown notifier %q", cfg.NotifierKind)
	}
}

// NewFromConfig builds the dispatcher both binaries run.
//
// A misconfigured transport fails HERE, at boot, rather than on the first
// booking. The alternative is a system that looks healthy and silently never
// notifies anybody, which is the failure mode operators find last.
func NewFromConfig(db *store.DB, cfg *config.Config, log *slog.Logger) (*Dispatcher, error) {
	notifier, err := NotifierFor(cfg, log)
	if err != nil {
		return nil, err
	}

	return NewDispatcher(db, Options{
		Notifier:  notifier,
		ListenDSN: cfg.DBListenURL,
		Logger:    log,
	}), nil
}
