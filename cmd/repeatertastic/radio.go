// Starting a radio: its driver, host, meshtasticd nodes, identities and links.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pb "github.com/ScotMesh/RepeaterTastic/api/meshtastic"
	"github.com/ScotMesh/RepeaterTastic/internal/config"
	"github.com/ScotMesh/RepeaterTastic/internal/links/mqtt"
	"github.com/ScotMesh/RepeaterTastic/internal/links/udp"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/nodes"
	"github.com/ScotMesh/RepeaterTastic/internal/phoneapi"
	"github.com/ScotMesh/RepeaterTastic/internal/radio"
	"github.com/ScotMesh/RepeaterTastic/internal/radio/kiss"
	"github.com/ScotMesh/RepeaterTastic/internal/radio/lazy"
	"github.com/ScotMesh/RepeaterTastic/internal/radio/null"
	"github.com/ScotMesh/RepeaterTastic/internal/radio/spi"
	"google.golang.org/protobuf/proto"
)

// radioRuntime is one radio's running stack.
type radioRuntime struct {
	rc      config.RadioConfig
	radio   radio.Radio
	host    *mesh.Host
	api     *phoneapi.Manager
	udp     *udp.Link
	mqtt    []*mqtt.Link
	hosting *nodes.Hosting // runs this radio's nodes on meshtasticd
}

// startRadio opens a radio's modem, builds its host and identities and starts its client
// API and UDP link. The host itself is run by the caller.
func startRadio(ctx context.Context, rc config.RadioConfig, index int, log *slog.Logger, uplinked *mqtt.Uplinked, sh *sensorHub) (*radioRuntime, error) {
	if err := os.MkdirAll(rc.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	r, board, err := openRadio(ctx, rc, log)
	if err != nil {
		return nil, err
	}
	host, err := mesh.NewHost(rc.MeshConfig(), r, log)
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	var relay *nodes.Node
	if board != nil {
		if relay, err = attachBoard(ctx, host, board); err != nil {
			_ = r.Close()
			return nil, err
		}
	}
	hosting := startHosting(ctx, rc, index, host, relay, log, sh)
	if err := loadIdentities(ctx, rc.Config, host, log); err != nil {
		_ = r.Close()
		return nil, err
	}
	api := phoneapi.NewManager(host, log)
	go api.Run(ctx)

	rt := &radioRuntime{rc: rc, radio: r, host: host, api: api, hosting: hosting}
	if err := rt.startLinks(ctx, uplinked, log); err != nil {
		_ = r.Close()
		return nil, err
	}
	return rt, nil
}

// openRadio opens a radio's driver. A board on Meshtastic firmware is returned as board too.
func openRadio(ctx context.Context, rc config.RadioConfig, log *slog.Logger) (radio.Radio, *nodes.BoardRadio, error) {
	switch rc.Radio.Driver {
	case nodes.BoardDriver:
		// A board on Meshtastic firmware: it is the radio's relay, and the identities reach the air
		// through its MQTT client proxy, a hop behind.
		logf := logfFor(log)
		board, err := nodes.OpenBoard(ctx, rc.Radio.Device, filepath.Join(rc.StateDir, "board"), logf)
		if err != nil {
			return nil, nil, err
		}
		return board, board, nil
	case "kiss", "spi":
		// One lazy radio for both drivers, so first-time setup can switch driver as well as device
		// before anything has opened (see web.followUnopenedDevices).
		logf := logfFor(log.With("driver", rc.Radio.Driver))
		open := modemOpener(rc.Radio.Baud, log)
		return lazy.New(open, radio.Info{Driver: rc.Radio.Driver, Device: rc.Radio.Device}, 5*time.Second, logf), nil, nil
	default:
		return null.New(), nil, nil
	}
}

// modemOpener opens a KISS modem, or a LoRa chip on SPI or a CH341 USB adapter described by a
// meshtasticd board file (a path, a built-in board name, or "auto").
func modemOpener(baud int, log *slog.Logger) func(ctx context.Context, driver, device string) (radio.Radio, error) {
	var logged string // the board last described, so retries don't repeat it
	return func(ctx context.Context, driver, device string) (radio.Radio, error) {
		logf := logfFor(log.With("driver", driver))
		if driver != "spi" {
			return kiss.Open(ctx, kiss.Options{Device: device, Baud: baud, Logf: logf})
		}
		b, src, err := spi.Resolve(device)
		if err != nil {
			return nil, err
		}
		if logged != src {
			logf("board %s from %s", b.Summary(), src)
			logged = src
		}
		return spi.Open(ctx, b, logf)
	}
}

// attachBoard makes a board on Meshtastic firmware the host's relay.
func attachBoard(ctx context.Context, host *mesh.Host, board *nodes.BoardRadio) (*nodes.Node, error) {
	board.SetContacts(func(num uint32) *pb.User {
		if e, ok := host.DB.Get(num); ok && e.User != nil {
			return proto.Clone(e.User).(*pb.User)
		}
		return nil
	})
	relay := board.Node()
	id, err := relay.Identity(ctx, 10*time.Second)
	if err == nil {
		id.IsRelay = true
		err = host.AddIdentity(id)
	}
	if err != nil {
		return nil, fmt.Errorf("the Meshtastic board: %w", err)
	}
	host.AddConfigApplier(relay)
	relay.Bind(host, id)
	go relay.Run(ctx)
	return relay, nil
}

// startLinks starts the radio's UDP multicast and MQTT links.
func (rt *radioRuntime) startLinks(ctx context.Context, uplinked *mqtt.Uplinked, log *slog.Logger) error {
	rc, host := rt.rc, rt.host
	if rc.Links.UDPMulticast.Enabled {
		if err := rt.startUDP(ctx, log); err != nil {
			return err
		}
	}
	origins := mqtt.NewOrigins()
	for _, mc := range rc.Links.MQTT {
		if !mc.Enabled {
			continue
		}
		l := mqtt.New(host, mqtt.Options{Name: mc.Name, Address: mc.Address, Username: mc.Username, Password: mc.Password,
			TLS: mc.TLS, Root: mc.Root, Mode: mc.Mode, Gateway: mc.Gateway, Format: mc.Format,
			UplinkChannels: mc.UplinkChannels, DownlinkChannels: mc.DownlinkChannels, ChannelSelection: mc.ChannelSelection,
			IgnoreConsent: mc.IgnoreConsent, RelayMQTT: mc.RelayMQTT, RelayHops: mc.RelayHops, CrossLink: mc.CrossLink,
			DownlinkPerMinute: mc.DownlinkPerMinute, UplinkPerMinute: mc.UplinkPerMinute, FirmwareVersion: phoneapi.FirmwareVersion,
			MapReport: mc.MapReport.Enabled, MapInterval: mc.MapReport.Interval, PositionPrecision: mc.MapReport.PositionPrecision,
			Latitude: mc.MapReport.Latitude, Longitude: mc.MapReport.Longitude, Altitude: mc.MapReport.Altitude,
			Origins: origins, Uplinked: uplinked}, log)
		rt.mqtt = append(rt.mqtt, l)
		go runLink(ctx, l.Run, log, "MQTT link stopped", "connection", mc.Name)
	}
	return nil
}

// startUDP starts the radio's UDP multicast link.
func (rt *radioRuntime) startUDP(ctx context.Context, log *slog.Logger) error {
	var groups []string
	if g := rt.rc.Links.UDPMulticast.Group; g != "" && !strings.Contains(g, ":") {
		groups = strings.Split(g, ",")
	}
	var err error
	if rt.udp, err = udp.New(rt.host, groups, 0, "", log); err != nil {
		return err
	}
	go runLink(ctx, rt.udp.Run, log, "UDP link stopped")
	return nil
}

// runLink runs a link until ctx ends, logging msg (with args) if it stops for another reason.
func runLink(ctx context.Context, run func(context.Context) error, log *slog.Logger, msg string, args ...any) {
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error(msg, append(args, "err", err)...)
	}
}

// loadIdentities restores identities from the state dir, or creates the relay persona and the
// identities listed in the config on first start. The host's hoster (if any) runs them on
// meshtasticd with their saved keys.
func loadIdentities(ctx context.Context, cfg *config.Config, host *mesh.Host, log *slog.Logger) error {
	recs, err := mesh.LoadIdentityRecords(cfg.StateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading identities: %w", err)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].IsRelay && !recs[j].IsRelay }) // the persona first
	boardRelay := host.Relay() != nil                                                          // a board is the relay
	restoreRecords(ctx, host, recs, boardRelay, log)
	if host.Relay() == nil && !keptRelay(recs, boardRelay) {
		if err := createRelay(ctx, cfg, host, log); err != nil {
			return err
		}
	}
	if len(recs) == 0 {
		if err := createConfigIdentities(ctx, cfg, host, log); err != nil {
			return err
		}
	}
	return host.SaveIdentities()
}

// restoreRecords starts the saved identities, keeping any that can't start (and the saved
// relay persona while a board is the relay) for the next start.
func restoreRecords(ctx context.Context, host *mesh.Host, recs []mesh.IdentityRecord, boardRelay bool, log *slog.Logger) {
	for _, rec := range recs {
		if rec.IsRelay && boardRelay {
			host.KeepRecord(rec) // back when the radio isn't a board any more
			continue
		}
		if _, err := host.AddRecord(ctx, rec); err != nil {
			log.Error("identity not started; kept for the next start", "node", rec.LongName, "err", err)
			host.KeepRecord(rec)
		}
	}
}

// createRelay makes a new relay persona from the config.
func createRelay(ctx context.Context, cfg *config.Config, host *mesh.Host, log *slog.Logger) error {
	relay, err := newUniqueIdentity(host, cfg.Relay.LongName, cfg.Relay.ShortName)
	if err != nil {
		return err
	}
	relay.IsRelay = true
	if _, err := host.AddRecord(ctx, relay.Record()); err != nil {
		log.Error("relay persona not started; kept for the next start", "node", relay.NodeID(), "err", err)
		host.KeepRecord(relay.Record())
	} else {
		log.Info("created relay persona", "node", relay.NodeID())
	}
	return nil
}

// createConfigIdentities makes the identities listed in the config, on first start.
func createConfigIdentities(ctx context.Context, cfg *config.Config, host *mesh.Host, log *slog.Logger) error {
	for _, ci := range cfg.Identities {
		id, err := newUniqueIdentity(host, ci.LongName, ci.ShortName)
		if err != nil {
			return err
		}
		id.APIPort, id.APIBind = ci.APIPort, ci.APIBind
		if _, err := host.AddRecord(ctx, id.Record()); err != nil {
			log.Error("identity not started; kept for the next start", "node", id.NodeID(), "err", err)
			host.KeepRecord(id.Record())
			continue
		}
		log.Info("created identity", "node", id.NodeID(), "name", ci.LongName, "api_port", ci.APIPort)
	}
	return nil
}

// keptRelay reports whether a saved relay persona couldn't be started (so no new one is made).
func keptRelay(recs []mesh.IdentityRecord, boardRelay bool) bool {
	if boardRelay {
		return false
	}
	for _, r := range recs {
		if r.IsRelay {
			return true
		}
	}
	return false
}

// newUniqueIdentity generates keys until the node's last byte is free locally and among known nodes.
func newUniqueIdentity(host *mesh.Host, long, short string) (*mesh.Identity, error) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		id, err := mesh.NewIdentity(nil, long, short)
		if err != nil {
			continue
		}
		if host.DB.LastByteCollision(id.NodeNum) == 0 {
			return id, nil
		}
	}
	return nil, errors.New("couldn't find a free node number")
}

// startHosting runs a radio's nodes on meshtasticd. A modem or HAT radio gets a LoRa air, and the
// host's hoster starts the relay persona and every other identity on it as their records are
// loaded. A board radio's air has the board as its relay, and its identities run a hop behind it.
// When meshtasticd can't run, the nodes keep trying (and the status bar says why).
func startHosting(ctx context.Context, rc config.RadioConfig, index int, host *mesh.Host, relay *nodes.Node, log *slog.Logger, sh *sensorHub) *nodes.Hosting {
	hc := rc.Hosted
	l := nodes.LauncherFor(hc.Meshtasticd, hc.DockerImage)
	logf := logfFor(log)
	relayLong, relayShort := rc.Relay.LongName, rc.Relay.ShortName
	air := nodes.NewLoRaAir(host, logf).WithRelay(relay)
	opts := nodes.HostingOptions{Launcher: l, Air: air, Radio: rc.ID,
		Dir: filepath.Join(rc.StateDir, "hosted"), PortBase: hc.RadioPortBase(index),
		RelayOwner: func() (string, string) { return relayLong, relayShort }, Logf: logf,
		NodeLogf: func(nodeID string) func(string, ...any) { return logfFor(log.With("identity", nodeID)) }}
	if sh != nil {
		opts.Sensors = sh // identities publish the host's sensors as their own
	}
	if relay != nil {
		opts.HopsBehind = 1
	}
	x := nodes.NewHosting(ctx, opts)
	host.SetHoster(x)
	vctx, cancel := context.WithTimeout(ctx, 90*time.Second) // docker may pull the image first
	v, err := nodes.CheckLauncher(vctx, l)
	cancel()
	x.SetLauncherCheck(v, err)
	if err != nil {
		log.Error("meshtasticd can't run: identities stay off air until it can", "radio", rc.ID, "launcher", l.Describe(), "err", err)
	} else {
		log.Info("nodes run on meshtasticd", "radio", rc.ID, "version", v, "launcher", l.Describe(),
			"behind_board", relay != nil, "ports_from", hc.RadioPortBase(index))
	}
	return x
}

// logfFor adapts a logger to the printf-style logging of the radio and node packages. A message
// marked "ERROR " or "WARN " (after its "component: " prefix) is logged at that level; lines
// meshtasticd printed as errors are warnings here.
func logfFor(l *slog.Logger) func(string, ...any) {
	return func(f string, a ...any) {
		msg := fmt.Sprintf(f, a...)
		level := slog.LevelInfo
		switch {
		case strings.Contains(msg, ": ERROR "):
			level, msg = slog.LevelError, strings.Replace(msg, ": ERROR ", ": ", 1)
		case strings.Contains(msg, ": WARN "):
			level, msg = slog.LevelWarn, strings.Replace(msg, ": WARN ", ": ", 1)
		case strings.Contains(msg, "ERROR |") || strings.Contains(msg, "CRIT |"):
			level = slog.LevelWarn
		}
		l.Log(context.Background(), level, msg)
	}
}
