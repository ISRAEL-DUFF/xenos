package mail

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/israel-duff/xenos/internal/config"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestProductionRefusesTheLogMailer(t *testing.T) {
	if _, err := New(config.Config{Env: "production"}, quiet); err == nil {
		t.Fatal("production without SMTP must not start: the log mailer writes account-takeover links to the journal")
	}
	m, err := New(config.Config{Env: "development"}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(LogMailer); !ok {
		t.Fatalf("development without SMTP: got %T", m)
	}
	if _, err := New(config.Config{SMTPHost: "smtp.example.com", SMTPPort: "587"}, quiet); err == nil {
		t.Fatal("SMTP without a from address must be refused")
	}
	if m, err := New(config.Config{Env: "production", SMTPHost: "smtp.example.com", SMTPPort: "587", MailFrom: "Xenos <no-reply@example.com>"}, quiet); err != nil {
		t.Fatal(err)
	} else if _, ok := m.(*SMTPMailer); !ok {
		t.Fatalf("got %T", m)
	}
}

func TestHeaderInjectionIsRefused(t *testing.T) {
	m := &SMTPMailer{Host: "127.0.0.1", Port: "1", From: "a@example.com"}
	for _, c := range []struct{ to, subject string }{
		{"victim@example.com\r\nBcc: evil@example.com", "hi"},
		{"victim@example.com", "hi\nBcc: evil@example.com"},
	} {
		err := m.Send(context.Background(), c.to, c.subject, "body")
		if err == nil || !strings.Contains(err.Error(), "line break") {
			t.Errorf("%q / %q: got %v, want the header refused before any connection", c.to, c.subject, err)
		}
	}
}
