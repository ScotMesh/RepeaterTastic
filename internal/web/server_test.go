package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/config"
	"github.com/ScotMesh/RepeaterTastic/internal/logbuf"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/phoneapi"
	"github.com/ScotMesh/RepeaterTastic/internal/radio"
	"github.com/ScotMesh/RepeaterTastic/internal/radio/null"
)

// testEnv is a single-radio web server whose internals the tests can reach.
type testEnv struct {
	s    *Server
	srv  *httptest.Server
	host *mesh.Host
	cfg  *config.Config
	logs *logbuf.Buffer
}

// newTestEnv starts a single-radio server on a running host. The config has a file path (in the
// temp dir), so saves go to disk; edit may change the options before the server is made.
func newTestEnv(t *testing.T, edit func(*Options)) *testEnv {
	t.Helper()
	return newRadioEnv(t, null.New(), true, edit)
}

// newRadioEnv is newTestEnv on a given radio; the host only runs if run is set.
func newRadioEnv(t *testing.T, rad radio.Radio, run bool, edit func(*Options)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = dir
	cfg.Radio.Driver = "none"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := mesh.NewHost(cfg.MeshConfig(), rad, log)
	if err != nil {
		t.Fatal(err)
	}
	h.SetHoster(&fakeHoster{remote: &fakeRemote{}})
	relay, _ := mesh.NewIdentity(nil, "Relay", "RLY")
	relay.IsRelay = true
	if err := h.AddIdentity(relay); err != nil {
		t.Fatal(err)
	}
	if run {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		t.Cleanup(func() { cancel(); <-done })
		go func() { _ = h.Run(ctx); close(done) }()
	}
	logs := logbuf.New(10)
	o := Options{Config: cfg, Host: h, API: phoneapi.NewManager(h, log), Logs: logs, Version: "test", Log: log}
	if edit != nil {
		edit(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &testEnv{s: s, srv: srv, host: h, cfg: cfg, logs: logs}
}

// signIn finishes setup and returns a session token.
func (e *testEnv) signIn(t *testing.T) string {
	t.Helper()
	return setupAndSignIn(t, e.srv)
}

// do sends a request with a raw body and returns the status and body.
func (e *testEnv) do(t *testing.T, method, path, tok, contentType, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestNewFailsOnUnreadableAuthFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = dir
	if _, err := New(Options{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil {
		t.Fatal("a corrupt auth file was accepted")
	}
}

func TestLoadAuthKeepsSavedStateAndFillsSecret(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"tokens":[{"id":"abc","name":"HA"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := loadAuth(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.f.JWTSecret == "" || len(a.Tokens()) != 1 || a.Tokens()[0].Name != "HA" {
		t.Fatalf("auth = %+v", a.f)
	}
	// A state dir that is a file can't hold auth.json.
	if _, err := loadAuth(filepath.Join(dir, "auth.json")); err == nil {
		t.Fatal("auth under a file loaded")
	}
}

func TestReadJSONAndErrors(t *testing.T) {
	env := newTestEnv(t, nil)
	tok := env.signIn(t)
	code, body := env.do(t, "POST", "/api/v1/tokens", tok, "application/json", "{bad")
	if code != http.StatusBadRequest || !strings.Contains(body, "invalid JSON") {
		t.Fatalf("bad JSON: %d %s", code, body)
	}
	if code, body := env.do(t, "GET", "/api/v1/nope", tok, "", ""); code != http.StatusNotFound || !strings.Contains(body, "no such endpoint") {
		t.Fatalf("unknown endpoint: %d %s", code, body)
	}
	se := errStatus(http.StatusTeapot, "short and stout")
	if se.Error() != "short and stout" {
		t.Fatalf("Error() = %q", se.Error())
	}
	rec := httptest.NewRecorder()
	writeStatusError(rec, se)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status error code = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	writeStatusError(rec, errors.New("plain"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("plain error code = %d", rec.Code)
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.7:5555"
	if got := clientIP(r); got != "192.0.2.7" {
		t.Fatalf("clientIP = %q", got)
	}
	r.RemoteAddr = "no-port"
	if got := clientIP(r); got != "no-port" {
		t.Fatalf("clientIP without port = %q", got)
	}
}

func TestSPACachesAssetsAndSendsSecurityHeaders(t *testing.T) {
	s := &Server{}
	h := securityHeaders(s.spa())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || rec.Header().Get(cacheControl) != "no-cache" {
		t.Fatalf("index: %d %q", rec.Code, rec.Header().Get(cacheControl))
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "SAMEORIGIN",
		"Referrer-Policy": "strict-origin-when-cross-origin"} {
		if rec.Header().Get(k) != v {
			t.Errorf("%s = %q, want %q", k, rec.Header().Get(k), v)
		}
	}
	asset := firstAsset(t)
	if asset == "" {
		t.Skip("the embedded GUI has no assets")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/"+asset, nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get(cacheControl), "immutable") {
		t.Fatalf("asset %s: %d %q", asset, rec.Code, rec.Header().Get(cacheControl))
	}
}

// firstAsset is the path of a file under dist/assets, or "".
func firstAsset(t *testing.T) string {
	t.Helper()
	entries, err := Dist.ReadDir("dist/assets")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			return "assets/" + e.Name()
		}
	}
	return ""
}

func TestRunServesUntilCancelled(t *testing.T) {
	port := freePort(t)
	env := newTestEnv(t, func(o *Options) {
		o.Config.Web.Bind, o.Config.Web.Port = "127.0.0.1", port
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- env.s.Run(ctx) }()
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/setup"
	resp := waitForServer(t, url)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("GET setup: %d %v", resp.StatusCode, resp.Header)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't stop")
	}
}

func TestRunFailsOnBusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	env := newTestEnv(t, func(o *Options) {
		o.Config.Web.Bind, o.Config.Web.Port = "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := env.s.Run(ctx); err == nil {
		t.Fatal("Run on a busy port succeeded")
	}
}

// freePort is a TCP port on 127.0.0.1 that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitForServer polls url until it answers.
func waitForServer(t *testing.T, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never answered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRadioHelpers(t *testing.T) {
	env := newTestEnv(t, nil)
	s := env.s
	if s.radioFor(nil) != s.radios[0] {
		t.Fatal("radioFor(nil) isn't the main radio")
	}
	r := httptest.NewRequest("GET", "/?radio=nope", nil)
	if s.radioFor(r) != s.radios[0] {
		t.Fatal("unknown ?radio= should fall back to the main radio")
	}
	if s.radioWithIdentity("garbage") != nil {
		t.Fatal("a bad node id found a radio")
	}
	stranger, _ := mesh.NewIdentity(nil, "Stranger", "STR")
	if s.radioOf(stranger) != s.radios[0] {
		t.Fatal("an unknown identity should be reported on the main radio")
	}
	if st := s.radioStats(context.Background()); !st.Connected {
		t.Fatalf("radio stats = %+v", st)
	}
}
