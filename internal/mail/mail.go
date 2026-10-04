// Package mail sends transactional email. Provider is undecided (open question
// in the plan), so only a logging implementation exists for now.
package mail

import (
	"context"
	"log/slog"
)

type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// LogMailer writes emails to the log instead of sending them (development).
type LogMailer struct{ Log *slog.Logger }

func (m LogMailer) Send(_ context.Context, to, subject, body string) error {
	m.Log.Info("email (not sent)", "to", to, "subject", subject, "body", body)
	return nil
}
