// Package config loads repeatertastic.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	pb "github.com/ScotMesh/RepeaterTastic/api/meshtastic"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/mtclient"
	"github.com/ScotMesh/RepeaterTastic/internal/phy"
	"github.com/ScotMesh/RepeaterTastic/internal/wire"
)

type Config struct {
	Radio      Radio      `yaml:"radio" json:"radio"`
	Mesh       Mesh       `yaml:"mesh" json:"mesh"`
	Relay      Relay      `yaml:"relay" json:"relay"`
	Airtime    Airtime    `yaml:"airtime" json:"airtime"`
	Links      Links      `yaml:"links" json:"links"`
	Web        Web        `yaml:"web" json:"web"`
	MDNS       MDNS       `yaml:"mdns" json:"mdns"`
	StateDir   string     `yaml:"state_dir" json:"state_dir"`
	LogLevel   string     `yaml:"log_level" json:"log_level"`
	Identities []Identity `yaml:"identities" json:"identities"`
	Position   Position   `yaml:"position" json:"position"`

	// Radios are additional radios on the same site, each on its own preset. The
	// top-level radio/mesh/relay/airtime/links/identities above are the "main" radio.
	Radios []RadioInstance `yaml:"radios,omitempty" json:"radios,omitempty"`
	Site   Site            `yaml:"site,omitempty" json:"site,omitempty"`
	// Plugins are separate programs that extend RepeaterTastic (docs/plugins.md).
	Plugins Plugins `yaml:"plugins" json:"plugins"`

	// Sensors offer host readings to hosted identities as their own sensors (docs/sensors.md).
	Sensors Sensors `yaml:"sensors,omitempty" json:"sensors"`
	// Hosted says how meshtasticd runs the nodes (docs/meshtasticd-nodes.md).
	Hosted Hosted `yaml:"hosted,omitempty" json:"hosted"`

	path string
}

// Plugins configures the plugin manager. Plugins are installed from the GUI, the CLI or by
// dropping a bundle into <dir>/inbox; Entries here pin a plugin's switch, permissions and
// settings so they can't be changed from the GUI.
type Plugins struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Dir holds installed plugins, their data and state ("" = <state_dir>/plugins).
	Dir string `yaml:"dir,omitempty" json:"dir,omitempty"`
	// Listen is a TCP address attached plugins connect to, e.g. "127.0.0.1:4450" ("" = off).
	// Managed plugins always use a Unix socket in Dir.
	Listen string `yaml:"listen,omitempty" json:"listen,omitempty"`
	// AllowURLInstall lets the GUI download bundles from a URL.
	AllowURLInstall bool `yaml:"allow_url_install" json:"allow_url_install"`
	// StoreURL is the plugin store the Browse tab lists ("" = the ScotMesh store, "off" = no
	// store at all). A store is only a list of names, logos and download addresses; the
	// checksum in it is what the daemon checks a downloaded bundle against.
	StoreURL string `yaml:"store_url,omitempty" json:"store_url,omitempty"`
	// MessagesPerHour and traceroutesPerHour cap what each plugin may transmit.
	MessagesPerHour    int           `yaml:"messages_per_hour" json:"messages_per_hour"`
	TraceroutesPerHour int           `yaml:"traceroutes_per_hour" json:"traceroutes_per_hour"`
	Entries            []PluginEntry `yaml:"entries,omitempty" json:"entries,omitempty"`
}

// PluginEntry pins one installed plugin. Settings values may use ${ENV_VAR}.
type PluginEntry struct {
	ID          string         `yaml:"id" json:"id"`
	Enabled     bool           `yaml:"enabled" json:"enabled"`
	Permissions []string       `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	Settings    map[string]any `yaml:"settings,omitempty" json:"settings,omitempty"`
}

// PluginDir is where plugins live.
func (c *Config) PluginDir() string {
	if c.Plugins.Dir != "" {
		return c.Plugins.Dir
	}
	return filepath.Join(c.StateDir, "plugins")
}

// RadioInstance is one extra radio: its own modem, preset, relay persona and identities.
type RadioInstance struct {
	ID         string     `yaml:"id" json:"id"`     // short and stable: names the state dir and ?radio= in the API
	Name       string     `yaml:"name" json:"name"` // shown in the GUI
	Radio      Radio      `yaml:"radio" json:"radio"`
	Mesh       Mesh       `yaml:"mesh" json:"mesh"`
	Relay      Relay      `yaml:"relay" json:"relay"`
	Airtime    Airtime    `yaml:"airtime" json:"airtime"`
	Links      Links      `yaml:"links" json:"links"`
	Identities []Identity `yaml:"identities" json:"identities"`
	Position   Position   `yaml:"position" json:"position"`
}

// Position is a fixed site location broadcast by the relay persona (or every identity).
type Position struct {
	Latitude  float64 `yaml:"latitude" json:"latitude"`
	Longitude float64 `yaml:"longitude" json:"longitude"`
	Altitude  int     `yaml:"altitude" json:"altitude"` // metres above sea level
	// PrecisionBits keeps that many bits of latitude/longitude: 32 = exact, 16 ≈ 360 m, 13 ≈ 3 km. 0 = 32.
	PrecisionBits int           `yaml:"precision_bits" json:"precision_bits"`
	Interval      time.Duration `yaml:"interval" json:"interval"` // 0 = 3h, minimum 30m
	// Identities is "relay" (default) or "all".
	Identities string `yaml:"identities" json:"identities"`
}

// Hosted says how RepeaterTastic runs meshtasticd: every radio's relay persona (unless the radio is
// a Meshtastic board) and identities are meshtasticd instances.
type Hosted struct {
	// Meshtasticd is the binary to run ("" = meshtasticd on PATH). It needs version 2.8 or newer.
	Meshtasticd string `yaml:"meshtasticd,omitempty" json:"meshtasticd"`
	// DockerImage runs the instances in Docker from this image instead (API published on
	// 127.0.0.1 only), e.g. meshtastic/meshtasticd:2.8.0.47db0e3-alpha-debian.
	DockerImage string `yaml:"docker_image,omitempty" json:"docker_image"`
	// PortBase is the first client API port for hosted nodes (default 4500); radio n (0 = main)
	// uses the block of 100 from PortBase + 100·n: its persona first, then its identities.
	// meshtasticd listens on every interface when run directly.
	PortBase int `yaml:"port_base,omitempty" json:"port_base"`
}

// RadioPortBase is the first client API port of radio index n's hosted nodes.
func (h Hosted) RadioPortBase(n int) int { return h.HostedPortBase() + 100*n }

// HostedPortBase is Hosted.PortBase with its default.
func (h Hosted) HostedPortBase() int {
	if h.PortBase == 0 {
		return 4500
	}
	return h.PortBase
}

// Site holds settings shared by every radio on the mast.
type Site struct {
	// DutyCyclePct caps the summed transmit airtime of all radios over the last hour,
	// on top of each radio's own limit. 0 = no site-wide cap.
	DutyCyclePct float64 `yaml:"duty_cycle_percent" json:"duty_cycle_percent"`
	// MainRadioName is what the GUI calls the top-level radio ("" = Main).
	MainRadioName string `yaml:"main_radio_name,omitempty" json:"main_radio_name,omitempty"`
}

// MainRadioID is the ID of the radio described by the top-level config.
const MainRadioID = "main"

// RadioConfig is one radio's view of the configuration: shared settings from the
// file, per-radio sections from the radio's own block, and its own state dir.
type RadioConfig struct {
	ID   string
	Name string
	*Config
}

// RadioConfigs lists every radio, main first. Extra radios get their own state dir
// (state_dir/radios/<id>) so identities and history never mix.
func (c *Config) RadioConfigs() []RadioConfig {
	mainName := c.Site.MainRadioName
	if mainName == "" {
		mainName = "Main"
	}
	out := []RadioConfig{{ID: MainRadioID, Name: mainName, Config: c}}
	for _, ri := range c.Radios {
		v := *c
		v.Radio, v.Mesh, v.Relay, v.Airtime, v.Links, v.Identities = ri.Radio, ri.Mesh, ri.Relay, ri.Airtime, ri.Links, ri.Identities
		v.Position = ri.Position
		v.StateDir = filepath.Join(c.StateDir, "radios", ri.ID)
		v.Radios = nil
		name := ri.Name
		if name == "" {
			name = ri.ID
		}
		out = append(out, RadioConfig{ID: ri.ID, Name: name, Config: &v})
	}
	return out
}

// normalizeRoles writes relay roles under their current names ("mute" → client_mute), so a saved
// config speaks Meshtastic's role names.
func (c *Config) normalizeRoles() {
	c.Relay.Role = mesh.NormalizeRelayRole(c.Relay.Role)
	c.Relay.Rebroadcast = strings.ToLower(c.Relay.Rebroadcast)
	for i := range c.Radios {
		c.Radios[i].Relay.Role = mesh.NormalizeRelayRole(c.Radios[i].Relay.Role)
		c.Radios[i].Relay.Rebroadcast = strings.ToLower(c.Radios[i].Relay.Rebroadcast)
	}
}

// fillRadioDefaults gives extra radios the defaults a top-level radio would get, inheriting
// the region and NodeInfo interval from the main radio. Extra relays default to mute:
// a new radio on a mast shouldn't start repeating until someone decides it should.
func (c *Config) fillRadioDefaults() {
	d := Default()
	for i := range c.Radios {
		c.fillOneRadioDefaults(&c.Radios[i], d)
	}
}

// fillOneRadioDefaults fills one extra radio's unset fields from d and the main radio.
func (c *Config) fillOneRadioDefaults(r *RadioInstance, d *Config) {
	if r.Radio.Driver == "" {
		r.Radio.Driver = d.Radio.Driver
	}
	if r.Radio.Baud == 0 {
		r.Radio.Baud = d.Radio.Baud
	}
	if r.Mesh.Region == "" {
		r.Mesh.Region = c.Mesh.Region
	}
	if r.Mesh.HopLimit == 0 {
		r.Mesh.HopLimit = d.Mesh.HopLimit
	}
	if r.Relay.Role == "" {
		r.Relay.Role = mesh.RoleClientMute
	}
	if r.Relay.LongName == "" {
		r.Relay.LongName = "RepeaterTastic " + r.ID + " Relay"
	}
	if r.Relay.ShortName == "" {
		r.Relay.ShortName = strings.ToUpper(r.ID)
		if len(r.Relay.ShortName) > 4 {
			r.Relay.ShortName = r.Relay.ShortName[:4]
		}
	}
	if r.Airtime.NodeInfoInterval == 0 {
		r.Airtime.NodeInfoInterval = c.Airtime.NodeInfoInterval
	}
}

var radioIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)

func (c *Config) validateRadios() error {
	seenID := map[string]bool{MainRadioID: true}
	seenDev := map[string]string{}
	seenPort := map[int]string{}
	for i, rc := range c.RadioConfigs() {
		if i > 0 {
			if err := rc.validateExtra(seenID); err != nil {
				return err
			}
		}
		if err := rc.claimDevice(seenDev); err != nil {
			return err
		}
		if err := rc.claimAPIPorts(seenPort); err != nil {
			return err
		}
	}
	return nil
}

// validateExtra checks an extra radio's ID is well formed and unused, then its sections.
func (rc RadioConfig) validateExtra(seenID map[string]bool) error {
	if !radioIDPattern.MatchString(rc.ID) {
		return fmt.Errorf("radios: id %q must be 1-24 lowercase letters, digits or dashes", rc.ID)
	}
	if seenID[rc.ID] {
		return fmt.Errorf("radios: id %q is used twice (%q is the top-level radio)", rc.ID, MainRadioID)
	}
	seenID[rc.ID] = true
	if err := rc.validateOne(); err != nil {
		return fmt.Errorf("radios[%s]: %w", rc.ID, err)
	}
	return nil
}

// claimDevice records the radio's device in seenDev, failing if another radio already has it.
func (rc RadioConfig) claimDevice(seenDev map[string]string) error {
	drv := rc.Radio.Driver
	if (drv != "kiss" && drv != "spi" && drv != "meshtastic") || rc.Radio.Device == "" {
		return nil
	}
	dev := rc.Radio.Device
	if addr, err := mtclient.TCPAddress(dev); drv == "meshtastic" && !mtclient.IsSerial(dev) && err == nil {
		dev = strings.ToLower(addr) // one board, one client
	} else if real, err := filepath.EvalSymlinks(dev); err == nil { // /dev/serial/by-id/… and /dev/ttyUSB0 can be one modem
		dev = real
	}
	if other, ok := seenDev[dev]; ok {
		return fmt.Errorf("radios %s and %s both use %s", other, rc.ID, rc.Radio.Device)
	}
	seenDev[dev] = rc.ID
	return nil
}

// claimAPIPorts records the radio's identity API ports in seenPort, failing on a clash.
func (rc RadioConfig) claimAPIPorts(seenPort map[int]string) error {
	for _, id := range rc.Identities {
		if id.APIPort <= 0 {
			continue
		}
		if other, ok := seenPort[id.APIPort]; ok {
			return fmt.Errorf("api_port %d is used by radios %s and %s", id.APIPort, other, rc.ID)
		}
		seenPort[id.APIPort] = rc.ID
	}
	return nil
}

type Radio struct {
	Driver string `yaml:"driver" json:"driver"` // kiss | spi | meshtastic | sim | none
	// Device is the serial port for kiss. For spi it is a meshtasticd board file
	// (/etc/meshtasticd/config.d/lora-….yaml), a built-in board name, or auto. For meshtastic it is
	// the serial port of a board running Meshtastic firmware, or a network board's address
	// (host or host:port): the board is the radio's relay and carries the identities, a hop behind.
	Device string `yaml:"device" json:"device"`
	Baud   int    `yaml:"baud" json:"baud"`
}

type Mesh struct {
	Region          string  `yaml:"region" json:"region"`
	Preset          string  `yaml:"preset" json:"preset"`
	PrimaryChannel  string  `yaml:"primary_channel" json:"primary_channel"`
	ChannelNum      int     `yaml:"channel_num" json:"channel_num"`
	OverrideFreqMHz float64 `yaml:"override_frequency_mhz" json:"override_frequency_mhz"`
	FreqOffsetMHz   float64 `yaml:"frequency_offset_mhz" json:"frequency_offset_mhz"`
	TxPowerDBm      int     `yaml:"tx_power_dbm" json:"tx_power_dbm"`
	HopLimit        uint32  `yaml:"hop_limit" json:"hop_limit"`
	// HwModel is the hardware identities advertise: "auto" or "" = the modem's board (Heltec V3 →
	// HELTEC_V3), or a Meshtastic HardwareModel name such as PORTDUINO or RAK4631.
	HwModel string `yaml:"hw_model" json:"hw_model"`
}

type Relay struct {
	// Role is a Meshtastic device role for the relay (client, client_base, client_mute, router,
	// router_late), or monitor or off. "mute" is read as client_mute.
	Role string `yaml:"role" json:"role"`
	// Rebroadcast is Meshtastic's rebroadcast mode: all (default), all_skip_decoding, local_only,
	// known_only, none or core_portnums_only. The built-in relay only honours none.
	Rebroadcast string `yaml:"rebroadcast,omitempty" json:"rebroadcast"`
	// Favorites are node IDs (!xxxxxxxx) the relay treats as its own when its role is client_base:
	// packets from or to them are relayed like router_late. This host's identities always count.
	Favorites []string `yaml:"favorites,omitempty" json:"favorites"`
	LongName  string   `yaml:"long_name" json:"long_name"`
	ShortName string   `yaml:"short_name" json:"short_name"`
}

type Airtime struct {
	DutyCyclePct      float64       `yaml:"duty_cycle_percent" json:"duty_cycle_percent"`
	OverrideDutyCycle bool          `yaml:"override_duty_cycle" json:"override_duty_cycle"`
	NodeInfoInterval  time.Duration `yaml:"nodeinfo_interval" json:"nodeinfo_interval"`
	IdentitySharePct  float64       `yaml:"identity_share_percent" json:"identity_share_percent"`
	// TelemetryInterval is how often the relay persona broadcasts device telemetry (uptime,
	// channel and TX airtime use). 0 = off; at least 30 minutes.
	TelemetryInterval time.Duration `yaml:"telemetry_interval,omitempty" json:"telemetry_interval,omitempty"`
}

type Links struct {
	LocalDMOverRF bool         `yaml:"local_dm_over_rf" json:"local_dm_over_rf"`
	UDPMulticast  UDPMulticast `yaml:"udp_multicast" json:"udp_multicast"`
	MQTT          MQTTLinks    `yaml:"mqtt" json:"mqtt"`
}

// MQTT modes.
const (
	MQTTGateway    = "gateway"     // uplink and downlink, consent respected, zero-hop rebroadcast
	MQTTUplinkOnly = "uplink_only" // uplink only, never subscribes
	MQTTMapOnly    = "map_only"    // map reports only
	MQTTMonitor    = "monitor"     // decoded JSON of chosen channels, no downlink
	MQTTBridge     = "bridge"      // both ways, private channels allowed: joins sites or private meshes
)

// Channel selection policies: which channels a connection carries.
const (
	ChannelsOverride = "override" // only the connection's own uplink/downlink lists
	ChannelsCombine  = "combine"  // the lists plus identity channels with uplink/downlink on
	ChannelsIdentity = "identity" // only identity channel flags (as the firmware and apps set them)
)

// MQTTLinks is a radio's broker connections. A single mapping (the old one-connection form)
// loads as a one-item list.
type MQTTLinks []MQTT

func (l *MQTTLinks) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.MappingNode:
		var one MQTT
		if err := n.Decode(&one); err != nil {
			return err
		}
		*l = MQTTLinks{one}
		return nil
	case yaml.SequenceNode:
		var many []MQTT
		if err := n.Decode(&many); err != nil {
			return err
		}
		*l = many
		return nil
	case 0:
		return nil
	}
	return fmt.Errorf("links.mqtt must be a connection or a list of connections")
}

// OKToMQTT reports whether any connection gives consent for our packets. Consent is for other
// gateways, so it applies with the connection switched off too.
func (l MQTTLinks) OKToMQTT() bool {
	for _, m := range l {
		if m.OKToMQTT {
			return true
		}
	}
	return false
}

// RelayMQTT reports whether any enabled connection lets broker packets be rebroadcast on air.
func (l MQTTLinks) RelayMQTT() bool {
	for _, m := range l {
		if m.Enabled && m.RelayMQTT {
			return true
		}
	}
	return false
}

// MQTT is one broker connection.
type MQTT struct {
	Name     string `yaml:"name" json:"name"`
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	Address  string `yaml:"address" json:"address"` // host:port
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"-"`
	TLS      bool   `yaml:"tls" json:"tls"`
	// Root is the topic prefix; "" = msh/<region>, as the apps default it.
	Root string `yaml:"root" json:"root"`
	// Mode is gateway (default), uplink_only, map_only, monitor or bridge.
	Mode string `yaml:"mode,omitempty" json:"mode"`
	// Gateway is the identity the connection speaks as: "" or "relay" = the relay persona, or a node id.
	Gateway string `yaml:"gateway,omitempty" json:"gateway"`
	// Format is encrypted (default; monitor defaults to json), json or both.
	Format string `yaml:"format,omitempty" json:"format"`
	// UplinkChannels / DownlinkChannels name the channels this connection carries.
	UplinkChannels   []string `yaml:"uplink_channels,omitempty" json:"uplink_channels"`
	DownlinkChannels []string `yaml:"downlink_channels,omitempty" json:"downlink_channels"`
	// ChannelSelection is override, combine or identity; "" = identity without lists, override with them.
	ChannelSelection string `yaml:"channel_selection,omitempty" json:"channel_selection"`
	// IgnoreConsent uplinks other nodes' packets even without OK_TO_MQTT (bridge only).
	IgnoreConsent bool `yaml:"ignore_consent,omitempty" json:"ignore_consent"`
	// OKToMQTT sets OK_TO_MQTT on our identities' packets: consent for other gateways to uplink
	// them (the firmware's lora.config_ok_to_mqtt). Any connection with it on sets it.
	OKToMQTT bool `yaml:"ok_to_mqtt" json:"ok_to_mqtt"`
	// RelayMQTT lets packets from this connection be rebroadcast on air (off = the firmware's
	// ignore_mqtt): broker traffic never costs airtime unless a connection opts in.
	RelayMQTT bool `yaml:"relay_mqtt" json:"relay_mqtt"`
	// RelayHops is how far a rebroadcast broker packet may travel on air: 0 = zero-hop.
	RelayHops int `yaml:"relay_hops,omitempty" json:"relay_hops"`
	// CrossLink lets packets from this connection go out on other connections that also allow it.
	CrossLink bool `yaml:"cross_link,omitempty" json:"cross_link"`
	// BridgeAcknowledged must be true for mode bridge: it can carry private channels off the mesh.
	BridgeAcknowledged bool `yaml:"bridge_acknowledged,omitempty" json:"bridge_acknowledged"`
	// DownlinkPerMinute / UplinkPerMinute cap packets in each direction (0 = 30 / 120).
	DownlinkPerMinute int       `yaml:"downlink_per_minute" json:"downlink_per_minute"`
	UplinkPerMinute   int       `yaml:"uplink_per_minute,omitempty" json:"uplink_per_minute"`
	MapReport         MapReport `yaml:"map_report" json:"map_report"`
}

// ModeOrDefault is the effective mode.
func (m MQTT) ModeOrDefault() string {
	if m.Mode == "" {
		return MQTTGateway
	}
	return m.Mode
}

// FormatOrDefault is the effective payload format.
func (m MQTT) FormatOrDefault() string {
	switch {
	case m.Format != "":
		return m.Format
	case m.ModeOrDefault() == MQTTMonitor:
		return "json"
	}
	return "encrypted"
}

// SelectionOrDefault is the effective channel selection policy.
func (m MQTT) SelectionOrDefault() string {
	switch {
	case m.ChannelSelection != "":
		return m.ChannelSelection
	case len(m.UplinkChannels) > 0 || len(m.DownlinkChannels) > 0:
		return ChannelsOverride
	}
	return ChannelsIdentity
}

func (m MQTT) validate() error {
	if err := m.validateChoices(); err != nil {
		return err
	}
	if m.IgnoreConsent && m.ModeOrDefault() != MQTTBridge {
		return errors.New("ignore_consent is only allowed on a bridge")
	}
	if g := strings.TrimSpace(m.Gateway); g != "" && g != "relay" && !gatewayIDPattern.MatchString(g) {
		return fmt.Errorf("gateway must be relay or a node id like !a1c40e07, not %q", m.Gateway)
	}
	if m.RelayHops < 0 || m.RelayHops > 7 {
		return errors.New("relay_hops must be 0-7")
	}
	if m.RelayHops > 0 && m.ModeOrDefault() != MQTTBridge {
		return errors.New("relay_hops above 0 is only allowed on a bridge; other connections rebroadcast zero-hop")
	}
	if m.Enabled && m.Address == "" {
		return errors.New("address is required when the connection is enabled")
	}
	if p := m.MapReport.PositionPrecision; p < 0 || p > 32 {
		return errors.New("map_report.position_precision must be 0-32")
	}
	return nil
}

// validateChoices checks the mode, format and channel selection are known values.
func (m MQTT) validateChoices() error {
	switch m.ModeOrDefault() {
	case MQTTGateway, MQTTUplinkOnly, MQTTMapOnly, MQTTMonitor:
	case MQTTBridge:
		if !m.BridgeAcknowledged {
			return errors.New("mode bridge can carry private channels off the mesh: set bridge_acknowledged to confirm")
		}
	default:
		return fmt.Errorf("mode must be gateway, uplink_only, map_only, monitor or bridge, not %q", m.Mode)
	}
	switch m.FormatOrDefault() {
	case "encrypted", "json", "both":
	default:
		return fmt.Errorf("format must be encrypted, json or both, not %q", m.Format)
	}
	switch m.SelectionOrDefault() {
	case ChannelsOverride, ChannelsCombine, ChannelsIdentity:
	default:
		return fmt.Errorf("channel_selection must be override, combine or identity, not %q", m.ChannelSelection)
	}
	return nil
}

var gatewayIDPattern = regexp.MustCompile(`^![0-9a-fA-F]{8}$`)

// MapReport periodically publishes the relay persona to the broker's map topic.
type MapReport struct {
	Enabled  bool          `yaml:"enabled" json:"enabled"`
	Interval time.Duration `yaml:"interval" json:"interval"` // 0 = 1h, minimum 15m
	// PositionPrecision is how many bits of latitude/longitude are kept (the firmware's
	// map_report_settings.position_precision): 14 ≈ 1.5 km, 16 ≈ 360 m. 0 = 14.
	PositionPrecision int     `yaml:"position_precision" json:"position_precision"`
	Latitude          float64 `yaml:"latitude" json:"latitude"`
	Longitude         float64 `yaml:"longitude" json:"longitude"`
	Altitude          int     `yaml:"altitude" json:"altitude"`
}

type UDPMulticast struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Group   string `yaml:"group" json:"group"`
}

// MDNS advertises each identity as _meshtastic._tcp so apps can discover it.
type MDNS struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

type Web struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Bind    string `yaml:"bind" json:"bind"`
	Port    int    `yaml:"port" json:"port"`
	// SessionTTL is how long a web login lasts.
	SessionTTL time.Duration `yaml:"session_ttl" json:"session_ttl"`
	// MapTileURL is the Leaflet tile template for the nodes map. The public OSM
	// servers' usage policy discourages heavy app use, so busy sites should
	// point this at their own or a commercial tile server.
	MapTileURL string `yaml:"map_tile_url" json:"map_tile_url"`
}

// DefaultMapTileURL is CARTO's Positron basemap (OpenStreetMap data). The OpenStreetMap tile
// servers refuse browsers on a LAN address: no Referer gets 403 and a private-IP Referer 400.
// CARTO takes its basemap key as ?key= (without one, tiles carry an "API key required" watermark).
const DefaultMapTileURL = "https://{s}.basemaps.cartocdn.com/light_all/{z}/{x}/{y}{r}.png?key={api_key}"

// LegacyMapTileURLs were defaults before; configs saved with them get the current default.
var LegacyMapTileURLs = []string{"https://tile.openstreetmap.org/{z}/{x}/{y}.png", "https://{s}.basemaps.cartocdn.com/light_all/{z}/{x}/{y}{r}.png",
	"https://{s}.basemaps.cartocdn.com/light_all/{z}/{x}/{y}{r}.png?api_key={api_key}"}

// Identity seeds a virtual node on first start; afterwards identities live in the state dir.
type Identity struct {
	LongName  string `yaml:"long_name" json:"long_name"`
	ShortName string `yaml:"short_name" json:"short_name"`
	APIPort   int    `yaml:"api_port" json:"api_port"`
	APIBind   string `yaml:"api_bind" json:"api_bind"`
}

// Default returns a working EU_868 LongFast configuration.
func Default() *Config {
	return &Config{
		Radio:    Radio{Driver: "kiss", Device: "/dev/ttyUSB0", Baud: 115200},
		Mesh:     Mesh{Region: "EU_868", Preset: "LONG_FAST", HopLimit: 3, TxPowerDBm: 20},
		Relay:    Relay{Role: mesh.RoleClient, LongName: "RepeaterTastic Relay", ShortName: "RPTR"},
		Airtime:  Airtime{NodeInfoInterval: 3 * time.Hour, IdentitySharePct: 25},
		Links:    Links{UDPMulticast: UDPMulticast{Group: "239.0.0.69:4403"}},
		Web:      Web{Enabled: true, Bind: "0.0.0.0", Port: 8080, SessionTTL: 7 * 24 * time.Hour, MapTileURL: DefaultMapTileURL},
		MDNS:     MDNS{Enabled: true},
		Plugins:  Plugins{Enabled: true, AllowURLInstall: true, MessagesPerHour: 30, TraceroutesPerHour: 12},
		StateDir: "/var/lib/repeatertastic",
		LogLevel: "info",
	}
}

// Load reads path over the defaults. A missing file yields defaults (and Path set, for saving).
func Load(path string) (*Config, error) {
	c := Default()
	c.path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.fillRadioDefaults()
	c.normalizeRoles()
	c.Links.MQTT.fillNames()
	for i := range c.Radios {
		c.Radios[i].Links.MQTT.fillNames()
	}
	c.path = path
	return c, c.Validate()
}

func (c *Config) Path() string { return c.path }

// Save writes the config back to its file.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config has no file path")
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func (c *Config) PresetValue() (phy.Preset, error) {
	v, ok := pb.Config_LoRaConfig_ModemPreset_value[strings.ToUpper(c.Mesh.Preset)]
	if !ok {
		return 0, fmt.Errorf("unknown preset %q", c.Mesh.Preset)
	}
	return phy.Preset(v), nil
}

func (c *Config) Validate() error {
	if err := c.validateOne(); err != nil {
		return err
	}
	switch strings.ToLower(c.LogLevel) {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be debug, info, warn or error, not %q", c.LogLevel)
	}
	if u := c.Web.MapTileURL; u != "" && (!strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") ||
		!strings.Contains(u, "{z}") || !strings.Contains(u, "{x}") || !strings.Contains(u, "{y}")) {
		return errors.New("web.map_tile_url must be an http(s) URL containing {z}, {x} and {y}")
	}
	if c.Site.DutyCyclePct < 0 || c.Site.DutyCyclePct > 100 {
		return fmt.Errorf("site.duty_cycle_percent must be between 0 and 100")
	}
	if c.Plugins.MessagesPerHour < 0 || c.Plugins.MessagesPerHour > 600 || c.Plugins.TraceroutesPerHour < 0 || c.Plugins.TraceroutesPerHour > 120 {
		return errors.New("plugins.messages_per_hour must be 0-600 and traceroutes_per_hour 0-120")
	}
	if u := c.Plugins.StoreURL; u != "" && u != "off" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return errors.New(`plugins.store_url must be an http(s) URL, or "off" to hide the store`)
	}
	seen := map[string]bool{}
	for _, e := range c.Plugins.Entries {
		if e.ID == "" || seen[e.ID] {
			return fmt.Errorf("plugins.entries: every entry needs a unique id (%q)", e.ID)
		}
		seen[e.ID] = true
	}
	if err := c.validateSensors(); err != nil {
		return err
	}
	return c.validateRadios()
}

// validateOne checks one radio's sections.
func (c *Config) validateOne() error {
	checks := []func() error{c.validateMeshRelay, c.validateHosted, c.validateRadio, c.validateHwModel, c.validatePosition, c.validateMQTTLinks}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// validateMeshRelay checks the preset, region, telemetry interval and relay section.
func (c *Config) validateMeshRelay() error {
	if _, err := c.PresetValue(); err != nil {
		return err
	}
	if _, ok := phy.Regions[strings.ToUpper(c.Mesh.Region)]; !ok {
		return fmt.Errorf("unknown region %q", c.Mesh.Region)
	}
	if iv := c.Airtime.TelemetryInterval; iv != 0 && iv < 30*time.Minute {
		return errors.New("airtime.telemetry_interval must be 0 (off) or at least 30m")
	}
	if !mesh.ValidRelayRole(c.Relay.Role) {
		return fmt.Errorf("relay.role must be client, client_base, client_mute, router, router_late, monitor or off, not %q", c.Relay.Role)
	}
	if _, ok := mesh.RebroadcastMode(c.Relay.Rebroadcast); !ok {
		return fmt.Errorf("relay.rebroadcast must be all, all_skip_decoding, local_only, known_only, none or core_portnums_only, not %q", c.Relay.Rebroadcast)
	}
	for _, f := range c.Relay.Favorites {
		if _, err := wire.ParseNodeID(f); err != nil {
			return fmt.Errorf("relay.favorites: %q isn't a node ID such as !a1b2c3d4", f)
		}
	}
	return nil
}

// validateHosted checks the hosted-node ports and meshtasticd program.
func (c *Config) validateHosted() error {
	if pb := c.Hosted.PortBase; pb != 0 && (pb < 1024 || pb > 64000) {
		return errors.New("hosted.port_base must be between 1024 and 64000")
	}
	if c.Hosted.RadioPortBase(len(c.Radios)+1) > 65536 {
		return errors.New("hosted.port_base is too high for this many radios (each takes 100 ports)")
	}
	if b := c.Hosted.Meshtasticd; b != "" && filepath.Base(b) != "meshtasticd" {
		return errors.New("hosted.meshtasticd must be a meshtasticd program (a path ending in /meshtasticd)")
	}
	if strings.ContainsAny(c.Hosted.DockerImage, " \t\n") || strings.HasPrefix(c.Hosted.DockerImage, "-") {
		return errors.New("hosted.docker_image must be an image name such as meshtastic/meshtasticd:2.8.0.47db0e3-alpha-debian")
	}
	return nil
}

// validateRadio checks the radio driver and the device it needs.
func (c *Config) validateRadio() error {
	switch c.Radio.Driver {
	case "kiss", "sim", "none":
	case "spi":
		if strings.TrimSpace(c.Radio.Device) == "" {
			return errors.New("radio.driver spi needs radio.device: a meshtasticd board file, a built-in board name such as MeshAdv-900M30S, or auto")
		}
	case "meshtastic":
		return validateMeshtasticDevice(strings.TrimSpace(c.Radio.Device))
	default:
		return fmt.Errorf("radio.driver must be kiss, spi, meshtastic or none, not %q", c.Radio.Driver)
	}
	return nil
}

// validateMeshtasticDevice checks a Meshtastic board's serial port or network address.
func validateMeshtasticDevice(dev string) error {
	if dev == "" {
		return errors.New("radio.driver meshtastic needs radio.device: the board's serial port (/dev/ttyACM0, /dev/serial/by-id/…) or a network board's address (host or host:port)")
	}
	if mtclient.IsSerial(dev) {
		return nil
	}
	if _, err := mtclient.TCPAddress(dev); err != nil {
		return fmt.Errorf("radio.device: %w", err)
	}
	return nil
}

// validateHwModel checks mesh.hw_model names a Meshtastic hardware model or auto.
func (c *Config) validateHwModel() error {
	name := strings.ToUpper(strings.TrimSpace(c.Mesh.HwModel))
	if name == "" || name == "AUTO" {
		return nil
	}
	if _, ok := pb.HardwareModel_value[name]; !ok {
		return fmt.Errorf("mesh.hw_model %q is not a Meshtastic hardware model (or auto)", c.Mesh.HwModel)
	}
	return nil
}

// validatePosition checks the fixed position section.
func (c *Config) validatePosition() error {
	if p := c.Position; p.Latitude < -90 || p.Latitude > 90 || p.Longitude < -180 || p.Longitude > 180 {
		return errors.New("position.latitude/longitude out of range")
	}
	if p := c.Position.PrecisionBits; p < 0 || p > 32 {
		return errors.New("position.precision_bits must be 0-32")
	}
	switch strings.ToLower(c.Position.Identities) {
	case "", "relay", "all":
	default:
		return errors.New(`position.identities must be "relay" or "all"`)
	}
	return nil
}

// validateMQTTLinks checks each broker connection and that their names are unique.
func (c *Config) validateMQTTLinks() error {
	names := map[string]bool{}
	for i, m := range c.Links.MQTT {
		label := m.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		if m.Name != "" && names[m.Name] {
			return fmt.Errorf("links.mqtt: two connections are named %q", m.Name)
		}
		names[m.Name] = true
		if err := m.validate(); err != nil {
			return fmt.Errorf("links.mqtt %s: %w", label, err)
		}
		if m.Enabled && m.MapReport.Enabled && m.MapReport.Latitude == 0 && m.MapReport.Longitude == 0 && !c.hasPosition() {
			return fmt.Errorf("links.mqtt %s: map_report needs a position (its own latitude/longitude or the radio's position:)", label)
		}
	}
	return nil
}

// hasPosition reports whether the radio has a fixed position set.
func (c *Config) hasPosition() bool {
	return c.Position.Latitude != 0 || c.Position.Longitude != 0
}

// MeshConfig converts to the host configuration.
func (c *Config) MeshConfig() mesh.Config {
	preset, _ := c.PresetValue()
	return mesh.Config{
		Region: strings.ToUpper(c.Mesh.Region), Preset: preset, PrimaryChannel: c.Mesh.PrimaryChannel,
		ChannelNum: c.Mesh.ChannelNum, OverrideFreqMHz: c.Mesh.OverrideFreqMHz, FreqOffsetMHz: c.Mesh.FreqOffsetMHz,
		TxPowerDBm: c.Mesh.TxPowerDBm, HopLimit: c.Mesh.HopLimit, RelayRole: mesh.NormalizeRelayRole(c.Relay.Role), Rebroadcast: strings.ToLower(c.Relay.Rebroadcast), Favorites: favoriteNums(c.Relay.Favorites),
		DutyCyclePct: c.Airtime.DutyCyclePct, OverrideDutyCycle: c.Airtime.OverrideDutyCycle,
		NodeInfoInterval: c.Airtime.NodeInfoInterval, LocalDMOverRF: c.Links.LocalDMOverRF, StateDir: c.StateDir,
		TelemetryInterval: c.Airtime.TelemetryInterval,
		OKToMQTT:          c.Links.MQTT.OKToMQTT(), IgnoreMQTT: !c.Links.MQTT.RelayMQTT(),
		HwModel: c.hwModel(),
		Position: mesh.FixedPosition{Latitude: c.Position.Latitude, Longitude: c.Position.Longitude,
			Altitude: int32(c.Position.Altitude), PrecisionBits: uint32(c.Position.PrecisionBits),
			Interval: c.Position.Interval, AllIdentities: strings.EqualFold(c.Position.Identities, "all")},
	}
}

// hwModel resolves mesh.hw_model; UNSET means "use the modem's board".
func (c *Config) hwModel() pb.HardwareModel {
	name := strings.ToUpper(strings.TrimSpace(c.Mesh.HwModel))
	if name == "" || name == "AUTO" {
		return pb.HardwareModel_UNSET
	}
	return pb.HardwareModel(pb.HardwareModel_value[name])
}

// MeshConfig is MeshConfig with this radio's ID set.
func (rc RadioConfig) MeshConfig() mesh.Config {
	m := rc.Config.MeshConfig()
	m.RadioID = rc.ID
	return m
}

// fillNames names unnamed connections mqtt, mqtt-2, ...
func (l MQTTLinks) fillNames() {
	used := map[string]bool{}
	for _, m := range l {
		used[m.Name] = true
	}
	n := 1
	for i := range l {
		if l[i].Name != "" {
			continue
		}
		for {
			name := "mqtt"
			if n > 1 {
				name = fmt.Sprintf("mqtt-%d", n)
			}
			n++
			if !used[name] {
				l[i].Name, used[name] = name, true
				break
			}
		}
	}
}

// FillRadioDefaults gives radios added at run time the defaults Load would have given them.
func (c *Config) FillRadioDefaults() {
	c.fillRadioDefaults()
	for i := range c.Radios {
		c.Radios[i].Links.MQTT.fillNames()
	}
}

// ApplyEnv applies container-friendly overrides on top of the file (they are not written back
// unless the config is saved from the GUI):
//
//	REPEATERTASTIC_STATE_DIR     state_dir
//	REPEATERTASTIC_RADIO_DEVICE  radio.device of the main radio
//	REPEATERTASTIC_WEB_PORT      web.port
func (c *Config) ApplyEnv() {
	if v := strings.TrimSpace(os.Getenv("REPEATERTASTIC_STATE_DIR")); v != "" {
		c.StateDir = v
	}
	if v := strings.TrimSpace(os.Getenv("REPEATERTASTIC_RADIO_DEVICE")); v != "" {
		c.Radio.Device = v
	}
	if v := strings.TrimSpace(os.Getenv("REPEATERTASTIC_WEB_PORT")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			c.Web.Port = p
		}
	}
}

// favoriteNums parses the relay's favourite node IDs, skipping any that don't parse.
func favoriteNums(ids []string) []uint32 {
	var out []uint32
	for _, id := range ids {
		if n, err := wire.ParseNodeID(id); err == nil {
			out = append(out, n)
		}
	}
	return out
}
