// Package mail sends transactional email: over SMTP in production (any provider that offers SMTP:
// Postmark, Resend, SES, Mailgun, ...), and to the log in development.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/israel-duff/xenos/internal/config"
)

type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// New picks the mailer. SMTP is used whenever it is configured. Without it, development gets the log
// mailer, and production refuses to start (the log mailer writes reset and verification links, which
// are account-takeover tokens, into the journal).
func New(cfg config.Config, log *slog.Logger) (Mailer, error) {
	if cfg.SMTPHost != "" {
		if cfg.MailFrom == "" {
			return nil, errors.New("XENOS_MAIL_FROM is required with XENOS_SMTP_HOST")
		}
		return &SMTPMailer{Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.MailFrom}, nil
	}
	if cfg.Env == "production" {
		return nil, errors.New("XENOS_ENV=production needs XENOS_SMTP_HOST: the log mailer must not be used")
	}
	log.Warn("no SMTP configured: emails are written to the log (development only)")
	return LogMailer{Log: log}, nil
}

// LogMailer writes emails to the log instead of sending them (development).
type LogMailer struct{ Log *slog.Logger }

func (m LogMailer) Send(_ context.Context, to, subject, body string) error {
	m.Log.Info("email (not sent)", "to", to, "subject", subject, "body", body)
	return nil
}

// SMTPMailer sends plain-text mail. Port 465 uses implicit TLS; any other port must offer STARTTLS,
// and the connection is refused otherwise, so credentials and tokens never cross the network in clear.
type SMTPMailer struct {
	Host, Port, User, Pass, From string
	Timeout                      time.Duration // default 20s
}

func (m *SMTPMailer) Send(ctx context.Context, to, subject, body string) error {
	if err := checkHeader(to, subject, m.From); err != nil {
		return err
	}
	timeout := m.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addr := net.JoinHostPort(m.Host, m.Port)
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tlsCfg := &tls.Config{ServerName: m.Host, MinVersion: tls.VersionTLS12}
	if m.Port == "465" {
		conn = tls.Client(conn, tlsCfg)
	}
	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: %w", err)
	}
	defer c.Close()
	if m.Port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: server does not offer STARTTLS; refusing to send in clear")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("smtp: %w", err)
		}
	}
	if m.User != "" {
		if err := c.Auth(smtp.PlainAuth("", m.User, m.Pass, m.Host)); err != nil {
			return fmt.Errorf("smtp: %w", err)
		}
	}
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return fmt.Errorf("smtp: bad from address: %w", err)
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	msg := "From: " + m.From + "\r\nTo: " + to + "\r\nSubject: " + subject +
		"\r\nDate: " + time.Now().UTC().Format(time.RFC1123Z) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n") + "\r\n"
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	return c.Quit()
}

// checkHeader refuses CR/LF in anything that goes into a header: it would let a caller inject headers.
func checkHeader(vals ...string) error {
	for _, v := range vals {
		if strings.ContainsAny(v, "\r\n") {
			return errors.New("smtp: line break in a mail header")
		}
	}
	return nil
}

// Verify connects, negotiates TLS and authenticates without sending anything: a deployment check.
func (m *SMTPMailer) Verify(ctx context.Context) error {
	timeout := m.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(m.Host, m.Port))
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tlsCfg := &tls.Config{ServerName: m.Host, MinVersion: tls.VersionTLS12}
	if m.Port == "465" {
		conn = tls.Client(conn, tlsCfg)
	}
	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()
	if m.Port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("server does not offer STARTTLS")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if m.User != "" {
		if err := c.Auth(smtp.PlainAuth("", m.User, m.Pass, m.Host)); err != nil {
			return err
		}
	}
	return c.Quit()
}
