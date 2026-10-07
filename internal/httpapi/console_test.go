package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/israel-duff/xenos/internal/proxmox"
)

// consoleEnv has a running VM that exists on the (fake) host.
func consoleEnv(t *testing.T) (*testEnv, *vmFixture, string, int64, *proxmox.Fake) {
	t.Helper()
	env := newTestEnv(t)
	u, path := runningVM(t, env, "a@x.co", 100*nanoDay)
	var id int64
	var vmid int
	if err := env.st.Pool.QueryRow(context.Background(), `SELECT id, proxmox_vmid FROM vms LIMIT 1`).Scan(&id, &vmid); err != nil {
		t.Fatal(err)
	}
	pve := proxmox.NewFake()
	pve.VMs[vmid] = &proxmox.FakeVM{ID: vmid, Running: true}
	env.srv.PVE = pve
	return env, u, path, id, pve
}

func dialConsole(t *testing.T, env *testEnv, c *client, path, session, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u, _ := url.Parse(env.ts.URL)
	hdr := http.Header{}
	var cookies []string
	for _, ck := range c.http.Jar.Cookies(u) {
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	hdr.Set("Cookie", strings.Join(cookies, "; "))
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	wsURL := "ws" + strings.TrimPrefix(env.ts.URL, "http") + path + "/console/ws?session=" + url.QueryEscape(session)
	return websocket.DefaultDialer.Dial(wsURL, hdr)
}

func openSession(t *testing.T, c *client, path string) (string, string) {
	t.Helper()
	code, out := c.do("POST", path+"/console", map[string]any{}, c.csrfHdr())
	if code != 200 {
		t.Fatalf("console = %d %v", code, out)
	}
	return out["session"].(string), out["password"].(string)
}

func TestConsoleBridgesBothWays(t *testing.T) {
	env, u, path, _, pve := consoleEnv(t)
	session, password := openSession(t, u.c, path)
	if !strings.HasPrefix(password, "PVEVNC:") {
		t.Fatalf("password %q", password)
	}
	conn, _, err := dialConsole(t, env, u.c, path, session, env.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, msg, err := conn.ReadMessage(); err != nil || string(msg) != "RFB 003.008\n" {
		t.Fatalf("first message %q %v", msg, err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := conn.ReadMessage(); err != nil || string(msg) != "echo:hello" {
		t.Fatalf("echo %q %v", msg, err)
	}
	if len(pve.Consoles) != 1 {
		t.Fatalf("host consoles: %d", len(pve.Consoles))
	}
	conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pve.Consoles[0].Close() // idempotent; waits for the bridge to have closed it
		if pve.Consoles[0].Closed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestConsoleSessionIsOneTimeAndOwnerBound(t *testing.T) {
	env, u, path, _, _ := consoleEnv(t)
	other := newVMUser(t, env, "b@x.co", 100*nanoDay)

	// Another account cannot open a console on this VM, nor use this account's session id.
	if code, _ := other.c.do("POST", path+"/console", map[string]any{}, other.c.csrfHdr()); code != 404 {
		t.Errorf("another user's console = %d, want 404", code)
	}
	session, _ := openSession(t, u.c, path)
	if _, resp, err := dialConsole(t, env, other.c, path, session, env.ts.URL); err == nil || resp.StatusCode != 404 && resp.StatusCode != 403 {
		t.Errorf("another user presenting the session: %v %v", err, resp)
	}
	// That attempt consumed it, so even the owner cannot reuse it.
	if _, resp, err := dialConsole(t, env, u.c, path, session, env.ts.URL); err == nil || resp.StatusCode != 403 {
		t.Errorf("a used session: %v %v, want 403", err, resp)
	}
	// An unauthenticated caller has no session at all.
	anon := newClient(t, env.ts.URL)
	if _, resp, err := dialConsole(t, env, anon, path, "x", env.ts.URL); err == nil || resp.StatusCode != 401 {
		t.Errorf("anonymous: %v %v, want 401", err, resp)
	}
}

func TestConsoleRefusesForeignOrigins(t *testing.T) {
	env, u, path, _, _ := consoleEnv(t)
	for _, origin := range []string{"", "https://evil.example"} {
		session, _ := openSession(t, u.c, path)
		if _, resp, err := dialConsole(t, env, u.c, path, session, origin); err == nil || resp.StatusCode != 403 {
			t.Errorf("origin %q: %v %v, want 403", origin, err, resp)
		}
	}
}

func TestConsoleNeedsARunningVMAndAHost(t *testing.T) {
	env, u, path, id, _ := consoleEnv(t)
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE vms SET state='stopped' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if code, _ := u.c.do("POST", path+"/console", map[string]any{}, u.c.csrfHdr()); code != 409 {
		t.Errorf("stopped VM = %d, want 409", code)
	}
	env.srv.PVE = nil
	if code, _ := u.c.do("POST", path+"/console", map[string]any{}, u.c.csrfHdr()); code != 503 {
		t.Errorf("no host = %d, want 503", code)
	}
}

func TestConsoleEndsWhenTheAccountIsSuspended(t *testing.T) {
	env, u, path, _, _ := consoleEnv(t)
	old := consoleRecheck
	consoleRecheck = 50 * time.Millisecond
	defer func() { consoleRecheck = old }()

	session, _ := openSession(t, u.c, path)
	conn, _, err := dialConsole(t, env, u.c, path, session, env.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, _ = conn.ReadMessage() // the RFB banner
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE users SET status='suspended'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("the console must close once the account no longer qualifies")
	}
}

func TestSecurityHeadersAllowOnlyTheOwnWebsocket(t *testing.T) {
	env := newTestEnv(t)
	env.srv.Cfg.PublicURL = "https://app.example.com"
	rec := &headerRecorder{h: http.Header{}}
	env.srv.securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, &http.Request{})
	csp := rec.h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' wss://app.example.com;") || !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("csp %q", csp)
	}
}

type headerRecorder struct{ h http.Header }

func (r *headerRecorder) Header() http.Header         { return r.h }
func (r *headerRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *headerRecorder) WriteHeader(int)             {}
