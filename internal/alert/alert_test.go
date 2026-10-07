package alert

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/israel-duff/xenos/internal/testutil"
)

type inbox struct {
	mu   sync.Mutex
	sent []string
}

func (m *inbox) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, to+"|"+subject+"|"+body)
	return nil
}

func newNotifier(t *testing.T) (*Notifier, *inbox, *time.Time) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	box := &inbox{}
	return &Notifier{Store: testutil.DB(t), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return now }, Mailer: box, ToEmail: "ops@x.co"}, box, &now
}

func TestCooldownSuppressesRepeats(t *testing.T) {
	n, box, now := newNotifier(t)
	ctx := context.Background()

	n.Notify(ctx, "pool-full", time.Hour, "pool at 85%")
	n.Notify(ctx, "pool-full", time.Hour, "pool at 86%")
	n.Notify(ctx, "other", time.Hour, "a different problem")
	if len(box.sent) != 2 {
		t.Fatalf("sent %d, want 2 (repeat suppressed, other key delivered): %v", len(box.sent), box.sent)
	}
	if !strings.Contains(box.sent[0], "ops@x.co|[Xenos alert] pool-full|pool at 85%") {
		t.Fatalf("unexpected email %q", box.sent[0])
	}

	*now = now.Add(61 * time.Minute)
	n.Notify(ctx, "pool-full", time.Hour, "pool at 90%")
	if len(box.sent) != 3 {
		t.Fatalf("expected a re-alert after the cooldown, sent=%d", len(box.sent))
	}
	n.Notify(ctx, "always", 0, "one")
	n.Notify(ctx, "always", 0, "two")
	if len(box.sent) != 5 {
		t.Fatalf("zero cooldown must always send, sent=%d", len(box.sent))
	}
}

func TestTelegramDelivery(t *testing.T) {
	var gotPath, gotChat, gotText string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotPath, gotChat, gotText = r.URL.Path, r.Form.Get("chat_id"), r.Form.Get("text")
	}))
	defer ts.Close()
	n, _, _ := newNotifier(t)
	n.Mailer, n.ToEmail = nil, ""
	n.TelegramToken, n.TelegramChat, n.BaseURL = "123:SECRET", "-100", ts.URL

	n.Notify(context.Background(), "k", time.Hour, "disk pool at 91%")
	if gotPath != "/bot123:SECRET/sendMessage" || gotChat != "-100" || gotText != "Xenos: disk pool at 91%" {
		t.Fatalf("telegram request: path=%q chat=%q text=%q", gotPath, gotChat, gotText)
	}
}

func TestTelegramErrorDoesNotLeakToken(t *testing.T) {
	n, _, _ := newNotifier(t)
	n.TelegramToken, n.TelegramChat, n.BaseURL = "123:SECRET", "-100", "http://127.0.0.1:1" // refused
	err := n.telegram(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error must exist and not contain the bot token: %v", err)
	}
}

func TestNotifyNeverFailsTheCaller(t *testing.T) {
	n, _, _ := newNotifier(t)
	n.Mailer = failingMailer{}
	n.Notify(context.Background(), "k", time.Hour, "x") // must not panic or block
	if n.Configured() != true {
		t.Fatal("email channel counts as configured")
	}
	n.Mailer, n.ToEmail = nil, ""
	if n.Configured() {
		t.Fatal("no channel configured")
	}
	n.Notify(context.Background(), "k2", time.Hour, "log only") // goes to the log without error
}

type failingMailer struct{}

func (failingMailer) Send(context.Context, string, string, string) error {
	return errors.New("smtp down")
}
