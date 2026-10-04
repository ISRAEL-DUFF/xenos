// Package alert sends operator notifications (Telegram and/or email) with a
// per-key cooldown so one ongoing problem produces one message, not one a minute.
package alert

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenos/internal/mail"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
)

type Notifier struct {
	Store *store.Store
	Log   *slog.Logger
	Now   func() time.Time

	// Telegram: set both, or neither. BaseURL defaults to the public Bot API.
	TelegramToken string
	TelegramChat  string
	BaseURL       string
	HTTP          *http.Client

	// Email fallback or addition: both must be set.
	Mailer  mail.Mailer
	ToEmail string
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// Configured reports whether any delivery channel is set up.
func (n *Notifier) Configured() bool {
	return (n.TelegramToken != "" && n.TelegramChat != "") || (n.Mailer != nil && n.ToEmail != "")
}

// Notify delivers text unless an alert with the same key was sent within cooldown.
// Delivery errors are logged, never returned: an alerting failure must not stop the caller.
// Without any channel the alert is written to the error log so journald still records it.
func (n *Notifier) Notify(ctx context.Context, key string, cooldown time.Duration, text string) {
	now := n.now()
	_, err := n.Store.Q.ClaimAlert(ctx, db.ClaimAlertParams{
		Key: key, LastSentAt: now, LastSentAt_2: now.Add(-cooldown)})
	if err == pgx.ErrNoRows {
		return // inside the cooldown
	} else if err != nil {
		n.Log.Error("alert dedupe failed; sending anyway", "key", key, "err", err)
	}
	n.Log.Error("ALERT", "key", key, "text", text)

	if n.TelegramToken != "" && n.TelegramChat != "" {
		if err := n.telegram(ctx, text); err != nil {
			n.Log.Error("telegram alert failed", "key", key, "err", err)
		}
	}
	if n.Mailer != nil && n.ToEmail != "" {
		if err := n.Mailer.Send(ctx, n.ToEmail, "[Xenos alert] "+key, text); err != nil {
			n.Log.Error("email alert failed", "key", key, "err", err)
		}
	}
}

func (n *Notifier) telegram(ctx context.Context, text string) error {
	base := strings.TrimRight(n.BaseURL, "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	if len(text) > 3500 {
		text = text[:3500] + "…"
	}
	form := url.Values{"chat_id": {n.TelegramChat}, "text": {"Xenos: " + text}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+n.TelegramToken+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c := n.HTTP
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return redact(err, n.TelegramToken)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("telegram: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// redact keeps the bot token (which is part of the URL) out of logs.
func redact(err error, token string) error {
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "<token>"))
}
