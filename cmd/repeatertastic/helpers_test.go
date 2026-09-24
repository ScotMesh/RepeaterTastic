package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ScotMesh/RepeaterTastic/internal/config"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/radio/null"
)

func newTestHost(t *testing.T) *mesh.Host {
	t.Helper()
	return newTestHostIn(t, t.TempDir())
}

func newTestHostIn(t *testing.T, stateDir string) *mesh.Host {
	t.Helper()
	h, err := mesh.NewHost(mesh.Config{Region: "EU_868", StateDir: stateDir}, null.New(), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	c := config.Default()
	c.StateDir = t.TempDir()
	c.Radio.Driver = "none"
	return c
}

func TestResolveMapAPIKey(t *testing.T) {
	old := mapAPIKey
	t.Cleanup(func() { mapAPIKey = old })
	tests := []struct{ env, built, key, source string }{
		{" envkey ", "builtin", "envkey", "environment"},
		{"", "builtin", "builtin", "built in"},
		{"", "", "", "none"},
	}
	for _, tc := range tests {
		t.Setenv("REPEATERTASTIC_MAP_API_KEY", tc.env)
		mapAPIKey = tc.built
		if k, s := resolveMapAPIKey(); k != tc.key || s != tc.source {
			t.Errorf("env %q built %q: got %q/%q", tc.env, tc.built, k, s)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/setup" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	u, _ := url.Parse(srv.URL)
	quiet(t)
	t.Setenv("REPEATERTASTIC_WEB_PORT", u.Port())
	if got := healthcheck(); got != 0 {
		t.Fatalf("healthy server: %d", got)
	}
	status = http.StatusServiceUnavailable
	if got := healthcheck(); got != 1 {
		t.Fatalf("503: %d", got)
	}
	srv.Close()
	if got := healthcheck(); got != 1 {
		t.Fatalf("closed server: %d", got)
	}
	t.Setenv("REPEATERTASTIC_WEB_PORT", "")
	if got := healthcheck(); got != 0 && got != 1 {
		t.Fatalf("default port: %d", got)
	}
}

func TestApplyStaged(t *testing.T) {
	quiet(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	if applyStaged(p) {
		t.Fatal("applied with nothing staged")
	}
	if err := os.WriteFile(p+".restore", []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !applyStaged(p) {
		t.Fatal("staged file not applied")
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("content %q", b)
	}
	// A staged file that can't replace its target (a non-empty directory) is left alone.
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked+".restore", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if applyStaged(blocked) {
		t.Fatal("rename over a directory reported as applied")
	}
}

func TestIsStopped(t *testing.T) {
	if !isStopped(nil) || !isStopped(context.Canceled) || !isStopped(errors.Join(errors.New("x"), context.Canceled)) {
		t.Fatal("clean stop not recognised")
	}
	if isStopped(errors.New("radio gone")) {
		t.Fatal("failure taken for a stop")
	}
}

// recordHandler keeps the level and message of every record.
type recordHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, r)
	h.mu.Unlock()
	return nil
}
func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordHandler) last() slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recs[len(h.recs)-1]
}

func TestLogfForLevels(t *testing.T) {
	h := &recordHandler{}
	logf := logfFor(slog.New(h))
	tests := []struct {
		format string
		level  slog.Level
		msg    string
	}{
		{"kiss: ERROR port %s gone", slog.LevelError, "kiss: port x gone"},
		{"node: WARN slow %s", slog.LevelWarn, "node: slow x"},
		{"meshtasticd[7] ERROR | %s", slog.LevelWarn, "meshtasticd[7] ERROR | x"},
		{"meshtasticd[7] CRIT | %s", slog.LevelWarn, "meshtasticd[7] CRIT | x"},
		{"plain %s", slog.LevelInfo, "plain x"},
	}
	for _, tc := range tests {
		logf(tc.format, "x")
		if r := h.last(); r.Level != tc.level || r.Message != tc.msg {
			t.Errorf("%q: got %v %q", tc.format, r.Level, r.Message)
		}
	}
}

func TestRunLinkLogsFailuresOnly(t *testing.T) {
	h := &recordHandler{}
	log := slog.New(h)
	runLink(context.Background(), func(context.Context) error { return context.Canceled }, log, "stopped")
	runLink(context.Background(), func(context.Context) error { return nil }, log, "stopped")
	if len(h.recs) != 0 {
		t.Fatalf("clean stops logged: %d", len(h.recs))
	}
	runLink(context.Background(), func(context.Context) error { return errors.New("refused") }, log, "link stopped", "connection", "c1")
	if r := h.last(); r.Level != slog.LevelError || r.Message != "link stopped" || r.NumAttrs() != 2 {
		t.Fatalf("got %v %q attrs %d", r.Level, r.Message, r.NumAttrs())
	}
}

func addIdentity(t *testing.T, h *mesh.Host, long string, relay, enabled bool, port int) *mesh.Identity {
	t.Helper()
	for {
		id, err := mesh.NewIdentity(nil, long, "X")
		if err != nil {
			t.Fatal(err)
		}
		id.IsRelay, id.Enabled, id.APIPort = relay, enabled, port
		if err := h.AddIdentity(id); err == nil {
			return id
		}
	}
}

func TestMDNSServices(t *testing.T) {
	h := newTestHost(t)
	addIdentity(t, h, "Relay", true, true, 4403)
	addIdentity(t, h, "Off", false, false, 4404)
	addIdentity(t, h, "No port", false, true, 0)
	desk := addIdentity(t, h, "Desk", false, true, 4405)
	svcs := mdnsServices([]*mesh.Host{h})
	if len(svcs) != 1 {
		t.Fatalf("services %+v", svcs)
	}
	s := svcs[0]
	if s.Port != 4405 || s.Instance != "Desk ("+desk.NodeID()+")" || s.TXT["id"] != desk.NodeID() || s.TXT["shortname"] != "X" {
		t.Fatalf("service %+v", s)
	}
}

func TestOnIdentityEvent(t *testing.T) {
	events := make(chan mesh.Event, 3)
	events <- mesh.Event{Type: "identity"}
	events <- mesh.Event{Type: "packet"}
	events <- mesh.Event{Type: "identity"}
	close(events)
	n := 0
	onIdentityEvent(events, func() { n++ })
	if n != 2 {
		t.Fatalf("called %d times, want 2", n)
	}
}

func TestPublishAllAndNewSite(t *testing.T) {
	a, b := newTestHost(t), newTestHost(t)
	radios := []*radioRuntime{{host: a}, {host: b}}
	ch, unsub := b.Bus.Subscribe(1)
	defer unsub()
	publishAll(radios, mesh.Event{Type: "log", Data: "hi"})
	if e := <-ch; e.Type != "log" || e.Data != "hi" {
		t.Fatalf("event %+v", e)
	}
	cfg := testConfig(t)
	if newSite(cfg, radios[:1], discardLog()) != nil {
		t.Fatal("site for a lone radio without a site budget")
	}
	cfg.Site.DutyCyclePct = 5
	if newSite(cfg, radios[:1], discardLog()) == nil {
		t.Fatal("no site with a site budget")
	}
}

func TestKeptRelay(t *testing.T) {
	relay := []mesh.IdentityRecord{{LongName: "a"}, {LongName: "r", IsRelay: true}}
	plain := []mesh.IdentityRecord{{LongName: "a"}}
	if !keptRelay(relay, false) || keptRelay(relay, true) || keptRelay(plain, false) {
		t.Fatal("wrong kept-relay verdict")
	}
}

func TestNewUniqueIdentityGivesUp(t *testing.T) {
	h := newTestHost(t)
	for b := uint32(0); b < 256; b++ {
		h.DB.Update(0x10000000|b, func(*mesh.NodeEntry) {})
	}
	// The relay persona can't be made, so loading fails.
	if err := loadIdentities(context.Background(), testConfig(t), h, discardLog()); err == nil {
		t.Fatal("found a node number with every last byte taken")
	}
}

func TestLoadIdentitiesWithoutHoster(t *testing.T) {
	// No hoster: nothing can start, so everything is kept for the next start and still saved.
	cfg := testConfig(t)
	h := newTestHostIn(t, cfg.StateDir)
	cfg.Identities = []config.Identity{{LongName: "Desk", ShortName: "DESK", APIPort: 4403}}
	if err := loadIdentities(context.Background(), cfg, h, discardLog()); err != nil {
		t.Fatal(err)
	}
	recs, err := mesh.LoadIdentityRecords(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRecord(recs, "RepeaterTastic Relay", true) || !hasRecord(recs, "Desk", false) {
		t.Fatalf("saved %+v", recs)
	}
	// Next start: the saved relay can't start either, and no new one is made.
	h2 := newTestHostIn(t, cfg.StateDir)
	if err := loadIdentities(context.Background(), cfg, h2, discardLog()); err != nil {
		t.Fatal(err)
	}
	again, _ := mesh.LoadIdentityRecords(cfg.StateDir)
	if len(again) != len(recs) {
		t.Fatalf("records %d -> %d", len(recs), len(again))
	}
}

func TestLoadIdentitiesKeepsRelayBehindBoard(t *testing.T) {
	h := newTestHost(t)
	board := addIdentity(t, h, "Board", true, true, 0)
	saved := []mesh.IdentityRecord{{LongName: "Old relay", IsRelay: true}, {LongName: "Desk"}}
	restoreRecords(context.Background(), h, saved, true, discardLog())
	if h.Relay() != board {
		t.Fatal("board is no longer the relay")
	}
	if err := h.SaveIdentities(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadIdentitiesBadFile(t *testing.T) {
	cfg := testConfig(t)
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "identities.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := loadIdentities(context.Background(), cfg, newTestHost(t), discardLog())
	if err == nil || !strings.Contains(err.Error(), "reading identities") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenRadioDrivers(t *testing.T) {
	cfg := testConfig(t)
	rc := config.RadioConfig{ID: "main", Config: cfg}
	ctx := context.Background()

	cfg.Radio.Driver = "kiss"
	cfg.Radio.Device = "/dev/nonexistent-tty"
	r, board, err := openRadio(ctx, rc, discardLog())
	if err != nil || board != nil {
		t.Fatalf("kiss: %v %v", err, board)
	}
	if in := r.Info(); in.Driver != "kiss" || in.Device != "/dev/nonexistent-tty" {
		t.Fatalf("info %+v", in)
	}
	_ = r.Close()

	cfg.Radio.Driver = "meshtastic"
	cfg.Radio.Device = "host:notaport"
	if _, _, err := openRadio(ctx, rc, discardLog()); err == nil {
		t.Fatal("bad board address accepted")
	}
}

func TestModemOpener(t *testing.T) {
	open := modemOpener(115200, discardLog())
	ctx := context.Background()
	if _, err := open(ctx, "kiss", filepath.Join(t.TempDir(), "tty")); err == nil {
		t.Fatal("missing serial port opened")
	}
	if _, err := open(ctx, "spi", filepath.Join(t.TempDir(), "board.yaml")); err == nil {
		t.Fatal("missing board file accepted")
	}
}

func TestStartRadioFailures(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = filepath.Join(blocker, "state")
	if _, err := startRadio(ctx, config.RadioConfig{ID: "main", Config: cfg}, 0, discardLog(), nil, nil); err == nil {
		t.Fatal("state dir under a file accepted")
	}

	cfg = testConfig(t)
	cfg.Radio.Driver, cfg.Radio.Device = "meshtastic", "host:notaport"
	if _, err := startRadios(ctx, cfg.RadioConfigs(), discardLog(), nil); err == nil || !strings.Contains(err.Error(), "radio main") {
		t.Fatalf("bad board: %v", err)
	}

	cfg = testConfig(t)
	cfg.Mesh.Region = "NOWHERE"
	if _, err := startRadio(ctx, config.RadioConfig{ID: "main", Config: cfg}, 0, discardLog(), nil, nil); err == nil {
		t.Fatal("unknown region accepted")
	}

	cfg = testConfig(t)
	cfg.Hosted.Meshtasticd = filepath.Join(t.TempDir(), "no-meshtasticd")
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "identities.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := startRadio(ctx, config.RadioConfig{ID: "main", Config: cfg}, 0, discardLog(), nil, nil); err == nil {
		t.Fatal("corrupt identities accepted")
	}

	cfg = testConfig(t)
	cfg.Hosted.Meshtasticd = filepath.Join(t.TempDir(), "no-meshtasticd")
	cfg.Links.UDPMulticast.Enabled = true
	cfg.Links.UDPMulticast.Group = "[bad"
	if _, err := startRadio(ctx, config.RadioConfig{ID: "main", Config: cfg}, 0, discardLog(), nil, nil); err == nil {
		t.Fatal("bad multicast group accepted")
	}
}

func TestRunConfigErrors(t *testing.T) {
	quiet(t)
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("radio: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(bad); err == nil {
		t.Fatal("bad yaml accepted")
	}
	state := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "c.yaml")
	cfg := "radio:\n  driver: meshtastic\n  device: host:notaport\nstate_dir: " + state + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err == nil {
		if err := run(cfgPath); err == nil {
			t.Fatal("run with a bad board address succeeded")
		}
	}
}

func TestPluginCommand(t *testing.T) {
	quiet(t)
	oldOut := os.Stdout
	os.Stdout = os.Stderr // quiet's /dev/null
	t.Cleanup(func() { os.Stdout = oldOut })
	cfgPath := filepath.Join(t.TempDir(), "c.yaml")
	cfg := "radio:\n  driver: none\nstate_dir: " + t.TempDir() + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := pluginCommand(cfgPath, []string{"permissions"}); got != 0 {
		t.Fatalf("permissions: %d", got)
	}
	if got := pluginCommand(cfgPath, []string{"no-such-command"}); got != 1 {
		t.Fatalf("unknown command: %d", got)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("log_level: loud\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := pluginCommand(bad, []string{"list"}); got != 1 {
		t.Fatalf("bad config: %d", got)
	}
}

func TestStartPluginsFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	radios := []*radioRuntime{{rc: config.RadioConfig{ID: "main"}, host: newTestHost(t)}}

	cfg := testConfig(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Plugins.Dir = filepath.Join(blocker, "plugins")
	if _, err := startPlugins(ctx, cfg, radios, nil, discardLog()); err == nil || !strings.HasPrefix(err.Error(), "plugins: ") {
		t.Fatalf("plugin dir under a file: %v", err)
	}

	// A manager that can't start is still returned: the repeater keeps running.
	cfg = testConfig(t)
	cfg.Plugins.Listen = "127.0.0.1:notaport"
	h := &recordHandler{}
	pm, err := startPlugins(ctx, cfg, radios, nil, slog.New(h))
	if err != nil || pm == nil {
		t.Fatalf("got %v %v", pm, err)
	}
	if pm.StartError() == "" || h.last().Message != "plugins couldn't start" {
		t.Fatalf("start error %q, last log %q", pm.StartError(), h.last().Message)
	}
}

func TestMainVersion(t *testing.T) {
	oldArgs, oldOut := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = oldArgs, oldOut })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"repeatertastic", "version"}
	os.Stdout = w
	main()
	os.Stdout = oldOut
	_ = w.Close()
	b, _ := io.ReadAll(r)
	if string(b) != "repeatertastic "+version+"\n" {
		t.Fatalf("printed %q", b)
	}
}
