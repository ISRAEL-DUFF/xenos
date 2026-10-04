package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/testutil"
	"github.com/israel-duff/xenos/web"
)

type captureMailer struct {
	mu   sync.Mutex
	last string
}

func (m *captureMailer) Send(_ context.Context, _, _, body string) error {
	m.mu.Lock()
	m.last = body
	m.mu.Unlock()
	return nil
}

var tokenRe = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

func (m *captureMailer) token(t *testing.T) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := tokenRe.FindStringSubmatch(m.last)
	if g == nil {
		t.Fatalf("no token in email %q", m.last)
	}
	return g[1]
}

type testEnv struct {
	ts     *httptest.Server
	mailer *captureMailer
	st     *store.Store
	ispend *billing.Fake
	srv    *Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st := testutil.DB(t)
	env := &testEnv{mailer: &captureMailer{}, st: st, ispend: billing.NewFake(150_000)}
	cfg := config.Config{PublicURL: "http://test", CookieSecure: false, Region: "test-1", ISpendWebhookSecret: testWebhookSecret}
	env.srv = NewServer(cfg, st, jobs.New(st.Pool), env.ispend, env.mailer,
		slog.New(slog.NewTextHandler(io.Discard, nil)), web.Dist())
	env.ts = httptest.NewServer(env.srv.Router())
	t.Cleanup(env.ts.Close)
	return env
}

func newTestServer(t *testing.T) (*httptest.Server, *captureMailer) {
	env := newTestEnv(t)
	return env.ts, env.mailer
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookieJar()
	return &client{t: t, base: base, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any, hdr map[string]string) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if tok, ok := out["csrf_token"].(string); ok && tok != "" {
		c.csrf = tok
	}
	return resp.StatusCode, out
}

func (c *client) csrfHdr() map[string]string { return map[string]string{"X-CSRF-Token": c.csrf} }

func TestAuthFlow(t *testing.T) {
	ts, mailer := newTestServer(t)
	c := newClient(t, ts.URL)
	signup := map[string]any{"email": "Ada@Example.com", "password": "correct-horse-1", "phone": "+2348012345678"}

	if code, _ := c.do("POST", "/v1/auth/signup", map[string]any{"email": "bad", "password": "x", "phone": "1"}, nil); code != 400 {
		t.Fatalf("invalid signup = %d", code)
	}
	code, out := c.do("POST", "/v1/auth/signup", signup, nil)
	if code != 201 {
		t.Fatalf("signup = %d %v", code, out)
	}
	if code, _ := c.do("POST", "/v1/auth/signup", signup, nil); code != 409 {
		t.Fatalf("duplicate signup = %d", code)
	}

	// Cookie session works; unsafe methods need CSRF.
	if code, out := c.do("GET", "/v1/auth/me", nil, nil); code != 200 || out["user"].(map[string]any)["email"] != "ada@example.com" {
		t.Fatalf("me = %d %v", code, out)
	}
	if code, _ := c.do("POST", "/v1/auth/resend-verification", nil, nil); code != 403 {
		t.Fatalf("missing CSRF should be 403, got %d", code)
	}

	// Email verification.
	tok := mailer.token(t)
	if code, _ := c.do("POST", "/v1/auth/verify", map[string]any{"token": "nope"}, nil); code != 400 {
		t.Fatalf("bad verify token = %d", code)
	}
	if code, _ := c.do("POST", "/v1/auth/verify", map[string]any{"token": tok}, nil); code != 200 {
		t.Fatalf("verify = %d", code)
	}
	if code, _ := c.do("POST", "/v1/auth/verify", map[string]any{"token": tok}, nil); code != 400 {
		t.Fatalf("verify token must be single-use, got %d", code)
	}
	if _, out := c.do("GET", "/v1/auth/me", nil, nil); out["user"].(map[string]any)["email_verified"] != true {
		t.Fatal("email should be verified")
	}

	// Logout invalidates the session.
	if code, _ := c.do("POST", "/v1/auth/logout", nil, c.csrfHdr()); code != 204 {
		t.Fatalf("logout = %d", code)
	}
	if code, _ := c.do("GET", "/v1/auth/me", nil, nil); code != 401 {
		t.Fatalf("me after logout = %d", code)
	}

	// Wrong password and unknown user look identical.
	c2, _ := c.do("POST", "/v1/auth/login", map[string]any{"email": "ada@example.com", "password": "wrong-password"}, nil)
	c3, _ := c.do("POST", "/v1/auth/login", map[string]any{"email": "ghost@example.com", "password": "wrong-password"}, nil)
	if c2 != 401 || c3 != 401 {
		t.Fatalf("bad logins = %d, %d", c2, c3)
	}

	// Bearer token: no CSRF needed, and a bearer token is not valid as a cookie.
	b := newClient(t, ts.URL)
	code, out = b.do("POST", "/v1/auth/login", map[string]any{"email": "ada@example.com", "password": "correct-horse-1", "token": true}, nil)
	if code != 200 || out["token"] == nil {
		t.Fatalf("bearer login = %d %v", code, out)
	}
	auth := map[string]string{"Authorization": "Bearer " + out["token"].(string)}
	if code, _ := b.do("GET", "/v1/auth/me", nil, auth); code != 200 {
		t.Fatalf("bearer me = %d", code)
	}
	if code, _ := b.do("POST", "/v1/auth/logout", nil, auth); code != 204 {
		t.Fatalf("bearer logout (no CSRF) = %d", code)
	}
	if code, _ := b.do("GET", "/v1/auth/me", nil, auth); code != 401 {
		t.Fatalf("revoked bearer = %d", code)
	}
}

func TestPasswordResetAndChange(t *testing.T) {
	ts, mailer := newTestServer(t)
	c := newClient(t, ts.URL)
	c.do("POST", "/v1/auth/signup", map[string]any{"email": "a@b.co", "password": "first-password-1", "phone": "+2348012345678"}, nil)

	// Unknown email still gets 202 and sends nothing new.
	if code, _ := c.do("POST", "/v1/auth/forgot-password", map[string]any{"email": "nobody@b.co"}, nil); code != 202 {
		t.Fatalf("forgot unknown = %d", code)
	}
	if code, _ := c.do("POST", "/v1/auth/forgot-password", map[string]any{"email": "a@b.co"}, nil); code != 202 {
		t.Fatalf("forgot = %d", code)
	}
	tok := mailer.token(t)
	if code, _ := c.do("POST", "/v1/auth/reset-password", map[string]any{"token": tok, "password": "second-password-2"}, nil); code != 200 {
		t.Fatalf("reset = %d", code)
	}
	if code, _ := c.do("POST", "/v1/auth/reset-password", map[string]any{"token": tok, "password": "third-password-3"}, nil); code != 400 {
		t.Fatalf("reset token reuse = %d", code)
	}
	// Reset signs everyone out, old password stops working.
	if code, _ := c.do("GET", "/v1/auth/me", nil, nil); code != 401 {
		t.Fatalf("session should be revoked, got %d", code)
	}
	if code, _ := c.do("POST", "/v1/auth/login", map[string]any{"email": "a@b.co", "password": "first-password-1"}, nil); code != 401 {
		t.Fatalf("old password = %d", code)
	}
	if code, _ := c.do("POST", "/v1/auth/login", map[string]any{"email": "a@b.co", "password": "second-password-2"}, nil); code != 200 {
		t.Fatalf("new password login = %d", code)
	}

	// Change password requires the current one and rotates the session.
	if code, _ := c.do("POST", "/v1/auth/change-password", map[string]any{"current": "nope", "new": "fourth-password-4"}, c.csrfHdr()); code != 401 {
		t.Fatalf("change with wrong current = %d", code)
	}
	if code, _ := c.do("POST", "/v1/auth/change-password", map[string]any{"current": "second-password-2", "new": "fourth-password-4"}, c.csrfHdr()); code != 200 {
		t.Fatalf("change = %d", code)
	}
	if code, _ := c.do("GET", "/v1/auth/me", nil, nil); code != 200 {
		t.Fatalf("fresh session after change = %d", code)
	}
}

func TestRateLimits(t *testing.T) {
	ts, _ := newTestServer(t)
	c := newClient(t, ts.URL)
	for i := 0; i < 3; i++ {
		c.do("POST", "/v1/auth/signup", map[string]any{"email": "u" + string(rune('a'+i)) + "@b.co", "password": "long-enough-pw", "phone": "+2348012345678"}, nil)
	}
	if code, _ := c.do("POST", "/v1/auth/signup", map[string]any{"email": "ud@b.co", "password": "long-enough-pw", "phone": "+2348012345678"}, nil); code != 429 {
		t.Fatalf("4th signup from one IP = %d", code)
	}
	for i := 0; i < 8; i++ {
		c.do("POST", "/v1/auth/login", map[string]any{"email": "ua@b.co", "password": "bad-password-x"}, nil)
	}
	if code, _ := c.do("POST", "/v1/auth/login", map[string]any{"email": "ua@b.co", "password": "long-enough-pw"}, nil); code != 429 {
		t.Fatalf("login after lockout = %d", code)
	}
}

func TestAdminAndAnonymousGuards(t *testing.T) {
	ts, _ := newTestServer(t)
	c := newClient(t, ts.URL)
	if code, _ := c.do("GET", "/v1/vms", nil, nil); code != 401 {
		t.Fatalf("anonymous /vms = %d", code)
	}
}
