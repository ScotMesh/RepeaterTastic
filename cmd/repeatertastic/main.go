// Command repeatertastic hosts many virtual Meshtastic nodes on one LoRa modem.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/config"
	"github.com/ScotMesh/RepeaterTastic/internal/links/mqtt"
	"github.com/ScotMesh/RepeaterTastic/internal/logbuf"
	"github.com/ScotMesh/RepeaterTastic/internal/mdns"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/nodes"
	"github.com/ScotMesh/RepeaterTastic/internal/plugins"
	"github.com/ScotMesh/RepeaterTastic/internal/site"
	"github.com/ScotMesh/RepeaterTastic/internal/web"
)

var version = "dev"

// mapAPIKey is the map tile provider's API key baked in at build time
// (-ldflags "-X main.mapAPIKey=..."). REPEATERTASTIC_MAP_API_KEY overrides it.
var mapAPIKey = ""

// resolveMapAPIKey picks the tile key and says where it came from (never the key itself).
func resolveMapAPIKey() (key, source string) {
	if k := strings.TrimSpace(os.Getenv("REPEATERTASTIC_MAP_API_KEY")); k != "" {
		return k, "environment"
	}
	if mapAPIKey != "" {
		return mapAPIKey, "built in"
	}
	return "", "none"
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("repeatertastic", version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	defaultConfig := "/etc/repeatertastic/repeatertastic.yaml"
	if p := strings.TrimSpace(os.Getenv("REPEATERTASTIC_CONFIG")); p != "" {
		defaultConfig = p
	}
	cfgPath := flag.String("config", defaultConfig, "configuration file (env REPEATERTASTIC_CONFIG)")
	flag.Parse()
	if flag.Arg(0) == "plugin" {
		os.Exit(pluginCommand(*cfgPath, flag.Args()[1:]))
	}
	if err := run(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "repeatertastic:", err)
		os.Exit(1)
	}
	if restartRequested.Load() {
		os.Exit(75) // EX_TEMPFAIL: the supervisor (systemd Restart=on-failure, Docker restart policy) starts it again
	}
}

// restartRequested is set when the web GUI asks for a restart: run shuts down cleanly (saving
// identities and closing modems), then main exits 75 so the supervisor starts it again.
var restartRequested atomic.Bool

func run(cfgPath string) error {
	restoredConfig := applyStaged(cfgPath) // a backup restored from the GUI replaces the config at start
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	cfg.ApplyEnv()
	logs := logbuf.New(2000)
	level, log := newLogger(cfg, logs)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if restoredConfig {
		log.Info("configuration restored from a backup")
	}
	applyStagedIdentities(cfg, log)

	// Always running, even with nothing configured: a sensor added in the GUI works without a restart.
	sh, err := newSensorHub(cfg.Sensors, log)
	if err != nil {
		return err
	}
	go sh.Run(ctx)
	if cfg.Sensors.Enabled() {
		log.Info("sensors", "sources", len(cfg.Sensors.Sources), "attachments", len(cfg.Sensors.Attach))
	}

	radios, err := startRadios(ctx, cfg.RadioConfigs(), log, sh)
	for _, rt := range radios {
		defer rt.radio.Close()
	}
	if err != nil {
		return err
	}
	logs.OnEntry(func(e logbuf.Entry) { publishAll(radios, mesh.Event{Type: "log", Data: e}) })

	// The radios of the mast know each other's identities.
	mesh.JoinSite(radioHosts(radios)...)
	st := newSite(cfg, radios, log)

	if cfg.MDNS.Enabled {
		go runMDNS(ctx, radioHosts(radios), log)
	}

	var pm *plugins.Manager
	if cfg.Plugins.Enabled {
		if pm, err = startPlugins(ctx, cfg, radios, sh, log); err != nil {
			return err
		}
		defer func() { stop(); pm.Wait() }() // plugins stop before the radios close
	}

	if cfg.Web.Enabled {
		opts := web.Options{Config: cfg, Logs: logs, LogLevel: level, Plugins: pm,
			Sensors: sh.Registry(), SensorsChanged: sh.Set,
			Restart: func() { restartRequested.Store(true); stop() },
			Site:    st, Version: version, Log: log}
		if err := startWeb(ctx, opts, radios, log); err != nil {
			return err
		}
	}

	return runRadios(ctx, stop, radios, log)
}

// newLogger builds the logger at the configured level, copying entries into logs for the GUI.
func newLogger(cfg *config.Config, logs *logbuf.Buffer) (*slog.LevelVar, *slog.Logger) {
	level := new(slog.LevelVar) // the web GUI changes it live
	var l slog.Level
	if l.UnmarshalText([]byte(strings.ToUpper(cfg.LogLevel))) == nil {
		level.Set(l)
	}
	log := slog.New(logbuf.NewHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}), logs))
	slog.SetDefault(log)
	return level, log
}

// applyStagedIdentities moves restored identity files into place before any radio loads them.
func applyStagedIdentities(cfg *config.Config, log *slog.Logger) {
	for _, rc := range cfg.RadioConfigs() {
		if applyStaged(filepath.Join(rc.StateDir, "identities.json")) {
			log.Info("identities restored from a backup", "radio", rc.ID)
		}
	}
}

// startRadios starts every radio in order. On failure it still returns the radios that
// started, so the caller can close them.
func startRadios(ctx context.Context, rcs []config.RadioConfig, log *slog.Logger, sh *sensorHub) ([]*radioRuntime, error) {
	uplinked := mqtt.NewUplinked() // one per site: a packet heard on two radios is published once
	var radios []*radioRuntime
	for _, rc := range rcs {
		rlog := log
		if len(rcs) > 1 {
			rlog = log.With("radio", rc.ID)
		}
		rt, err := startRadio(ctx, rc, len(radios), rlog, uplinked, sh)
		if err != nil {
			return radios, fmt.Errorf("radio %s: %w", rc.ID, err)
		}
		radios = append(radios, rt)
	}
	return radios, nil
}

// publishAll puts e on every radio's event bus.
func publishAll(radios []*radioRuntime, e mesh.Event) {
	for _, rt := range radios {
		rt.host.Bus.Publish(e)
	}
}

// radioHosts lists the radios' mesh hosts.
func radioHosts(radios []*radioRuntime) []*mesh.Host {
	hosts := make([]*mesh.Host, 0, len(radios))
	for _, rt := range radios {
		hosts = append(hosts, rt.host)
	}
	return hosts
}

// newSite makes the site coordinator whenever several radios share a mast (co-channel transmit
// turns) or a site-wide airtime budget is set, and nil otherwise.
func newSite(cfg *config.Config, radios []*radioRuntime, log *slog.Logger) *site.Site {
	if len(radios) <= 1 && cfg.Site.DutyCyclePct <= 0 {
		return nil
	}
	st := site.New(cfg.Site.DutyCyclePct)
	for _, rt := range radios {
		st.Add(rt.host)
	}
	for _, rt := range radios {
		o := st.Overlaps(rt.host)
		if len(o) == 0 {
			continue
		}
		ids := make([]string, 0, len(o))
		for _, h := range o {
			ids = append(ids, h.RadioID())
		}
		log.Warn("radio shares its channel with other radios on this site; they will take turns to transmit",
			"radio", rt.rc.ID, "overlaps", strings.Join(ids, ","))
	}
	return st
}

// startPlugins creates and starts the plugin manager. Only a manager that can't be created is
// an error: a repeater keeps repeating even if plugins can't start.
func startPlugins(ctx context.Context, cfg *config.Config, radios []*radioRuntime, sh *sensorHub, log *slog.Logger) (*plugins.Manager, error) {
	prs := make([]plugins.Radio, 0, len(radios))
	for _, rt := range radios {
		prs = append(prs, plugins.Radio{ID: rt.rc.ID, Name: rt.rc.Name, Host: rt.host})
	}
	pm, err := plugins.New(plugins.Options{Config: cfg.Plugins, Dir: cfg.PluginDir(), Radios: prs, Version: version, Log: log,
		Sensors: sh.Registry(),
		Notify:  func(id string) { publishAll(radios, mesh.Event{Type: "plugin", Data: id}) }})
	if err != nil {
		return nil, fmt.Errorf("plugins: %w", err)
	}
	// Say why in the log and the GUI.
	if err := pm.Start(ctx); err != nil {
		log.Error("plugins couldn't start", "err", err)
	}
	return pm, nil
}

// startWeb fills opts with the radios and map key, then runs the web server in the background.
func startWeb(ctx context.Context, opts web.Options, radios []*radioRuntime, log *slog.Logger) error {
	primary := radios[0]
	extra := make([]web.Radio, 0, len(radios)-1)
	for _, rt := range radios[1:] {
		extra = append(extra, web.Radio{ID: rt.rc.ID, Name: rt.rc.Name, Config: rt.rc.Config, Host: rt.host, API: rt.api, UDP: rt.udp, MQTT: rt.mqtt})
	}
	hostings := map[string]*nodes.Hosting{}
	for _, rt := range radios {
		hostings[rt.rc.ID] = rt.hosting
	}
	key, source := resolveMapAPIKey()
	log.Info("map tiles", "api_key", source)
	opts.Host, opts.API, opts.UDP, opts.MQTT = primary.host, primary.api, primary.udp, primary.mqtt
	opts.MapAPIKey, opts.MapKeySource = key, source
	opts.Hosting, opts.Radios = hostings, extra
	srv, err := web.New(opts)
	if err != nil {
		return err
	}
	go func() {
		if err := srv.Run(ctx); err != nil {
			log.Error("web server stopped", "err", err)
		}
	}()
	return nil
}

// runRadios runs every radio until one stops, stops the rest and saves identities.
func runRadios(ctx context.Context, stop context.CancelFunc, radios []*radioRuntime, log *slog.Logger) error {
	primary := radios[0]
	for _, rt := range radios {
		rp := rt.host.RadioParams()
		log.Info("RepeaterTastic starting", "version", version, "radio", rt.rc.ID, "region", rp.Region.Name, "preset", rp.PresetName(),
			"freq_mhz", rp.FrequencyMHz, "identities", len(rt.host.Identities()))
	}
	errs := make(chan error, len(radios))
	for _, rt := range radios[1:] {
		go func() { errs <- rt.host.Run(ctx) }()
	}
	err := primary.host.Run(ctx)
	stop() // one radio stopping stops the others
	for range radios[1:] {
		if e := <-errs; isStopped(err) {
			err = e
		}
	}
	for _, rt := range radios {
		_ = rt.host.SaveIdentities()
	}
	if isStopped(err) {
		log.Info("stopped")
		return nil
	}
	return err
}

// isStopped reports whether err means a clean stop rather than a failure.
func isStopped(err error) bool {
	return err == nil || errors.Is(err, context.Canceled)
}

// runMDNS keeps the advertised _meshtastic._tcp services in step with every radio's identities.
func runMDNS(ctx context.Context, hosts []*mesh.Host, log *slog.Logger) {
	r := mdns.New(log)
	update := func() { r.SetServices(mdnsServices(hosts)) }
	update()
	for _, host := range hosts {
		events, unsub := host.Bus.Subscribe(16)
		defer unsub()
		go onIdentityEvent(events, update)
	}
	if err := r.Run(ctx); err != nil {
		log.Warn("mDNS advertising disabled", "err", err)
	}
}

// mdnsServices lists a service for every enabled identity with a client API port.
func mdnsServices(hosts []*mesh.Host) []mdns.Service {
	var svcs []mdns.Service
	for _, host := range hosts {
		for _, id := range host.Identities() {
			if id.IsRelay || !id.Enabled || id.APIPort <= 0 {
				continue
			}
			u := id.UserCopy()
			svcs = append(svcs, mdns.Service{Instance: fmt.Sprintf("%s (%s)", u.LongName, id.NodeID()), Port: id.APIPort,
				TXT: map[string]string{"id": id.NodeID(), "shortname": u.ShortName, "pio_env": "repeatertastic"}})
		}
	}
	return svcs
}

// onIdentityEvent calls fn for each identity event until events closes.
func onIdentityEvent(events <-chan mesh.Event, fn func()) {
	for e := range events {
		if e.Type == "identity" {
			fn()
		}
	}
}

// healthcheck is `repeatertastic healthcheck` for container health checks: the web server answers
// on 127.0.0.1 (port from REPEATERTASTIC_WEB_PORT, default 8080). Exit status 0 = healthy.
func healthcheck() int {
	port := strings.TrimSpace(os.Getenv("REPEATERTASTIC_WEB_PORT"))
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/api/v1/setup")
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy: HTTP", resp.StatusCode)
		return 1
	}
	return 0
}

// applyStaged moves path+".restore" (written by a backup restore) over path. It reports whether it did.
func applyStaged(path string) bool {
	staged := path + ".restore"
	if _, err := os.Stat(staged); err != nil {
		return false
	}
	if err := os.Rename(staged, path); err != nil {
		fmt.Fprintln(os.Stderr, "repeatertastic: applying restored", path+":", err)
		return false
	}
	return true
}

// pluginCommand runs "repeatertastic plugin ...".
func pluginCommand(cfgPath string, args []string) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repeatertastic:", err)
		return 1
	}
	cfg.ApplyEnv()
	if err := plugins.CLI(cfg, args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "repeatertastic plugin:", err)
		return 1
	}
	return 0
}
