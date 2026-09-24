package nodes

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/mtclient"
)

// MinFirmware is the oldest meshtasticd a hosted node may run: 2.7 can't take a PKI DM back in
// through the sim envelope (docs/meshtasticd-nodes.md).
const MinFirmware = "2.8.0"

// Instance is one hosted meshtasticd.
type Instance struct {
	Name string // stable and short: names the container and the log lines
	Dir  string // config.yaml and the node's filesystem (vfs) live here
	Port int    // client API port
	HWID string // 12 hex digits: the node's MAC address
	// Env is extra environment for the process ("K=V"), the I²C shim's for a node that carries
	// sensors. Its paths are the ones that process sees: inside the container under Docker.
	Env []string
	// Sensors, when set, are the imitated I²C sensors the node finds when it starts (sensors.go).
	Sensors *SensorSetup
}

// ConfigPath is the instance's meshtasticd config file.
func (in Instance) ConfigPath() string { return filepath.Join(in.Dir, "config.yaml") }

// Args are meshtasticd's arguments with the given paths for the config file and filesystem.
func (in Instance) Args(config, vfs string) []string {
	return []string{"-c", config, "-d", vfs, "-h", in.HWID, "-p", strconv.Itoa(in.Port)}
}

// HWIDFor derives a stable, locally administered MAC address from a name.
func HWIDFor(name string) string {
	return fmt.Sprintf("02%010X", uint64(crc32.ChecksumIEEE([]byte(name)))|uint64(0xA5)<<32)
}

// Launcher runs meshtasticd.
type Launcher interface {
	// Run runs the instance until it exits or ctx ends.
	Run(ctx context.Context, in Instance, out io.Writer) error
	// Version reports the meshtasticd version the launcher runs.
	Version(ctx context.Context) (string, error)
	// Describe names the launcher for logs and the GUI.
	Describe() string
}

// ExecLauncher runs a meshtasticd binary directly. Its client API listens on every interface:
// meshtasticd has no bind option, so firewall the port range on a shared network.
type ExecLauncher struct {
	Binary string // path or name on PATH; "" = meshtasticd
}

func (l ExecLauncher) bin() string {
	if l.Binary == "" {
		return "meshtasticd"
	}
	return l.Binary
}

func (l ExecLauncher) Describe() string { return l.bin() }

func (l ExecLauncher) Run(ctx context.Context, in Instance, out io.Writer) error {
	cmd := exec.CommandContext(ctx, l.bin(), in.Args(in.ConfigPath(), filepath.Join(in.Dir, "vfs"))...)
	cmd.Dir = in.Dir
	if len(in.Env) > 0 {
		cmd.Env = append(os.Environ(), in.Env...)
	}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

func (l ExecLauncher) Version(ctx context.Context) (string, error) {
	return versionFrom(exec.CommandContext(ctx, l.bin(), "--version"))
}

// DockerLauncher runs meshtasticd in a container from a meshtasticd image, its API published on
// 127.0.0.1 only. For hosts without the meshtasticd package.
type DockerLauncher struct {
	Image string
}

func (l DockerLauncher) Describe() string { return "docker " + l.Image }

func (l DockerLauncher) container(in Instance) string {
	return "repeatertastic-" + in.Name + "-" + strconv.Itoa(in.Port)
}

func (l DockerLauncher) Run(ctx context.Context, in Instance, out io.Writer) error {
	name := l.container(in)
	dir, err := filepath.Abs(in.Dir) // docker takes a relative path for a volume name
	if err != nil {
		return err
	}
	_ = exec.Command("docker", "rm", "-f", name).Run() // a container left by a crash
	args := []string{"run", "--rm", "--name", name, "--label", "repeatertastic.hosted=1",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", in.Port, in.Port),
		"-v", dir + ":/data"}
	for _, e := range in.Env {
		// The instance directory is already mounted at /data, so the shim, its readings file and
		// the bus it fakes are all in there; Env's paths say /data.
		args = append(args, "-e", e)
	}
	args = append(args, l.Image, "/usr/bin/meshtasticd")
	args = append(args, in.Args("/data/config.yaml", "/data/vfs")...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Cancel = func() error { return exec.Command("docker", "stop", "-t", "3", name).Run() }
	cmd.WaitDelay = 10 * time.Second
	return cmd.Run()
}

func (l DockerLauncher) Version(ctx context.Context) (string, error) {
	return versionFrom(exec.CommandContext(ctx, "docker", "run", "--rm", l.Image, "/usr/bin/meshtasticd", "--version"))
}

var versionRE = regexp.MustCompile(`\b(\d+\.\d+\.\d+)(\.[0-9a-f]+)?`)

func versionFrom(cmd *exec.Cmd) (string, error) {
	b, err := cmd.CombinedOutput()
	if m := versionRE.FindString(string(b)); m != "" {
		return m, nil
	}
	if err != nil {
		return "", fmt.Errorf("meshtasticd --version: %w", err)
	}
	return "", errors.New("meshtasticd --version printed no version")
}

// VersionAtLeast compares dotted versions by their first three numbers.
func VersionAtLeast(v, min string) bool {
	parse := func(s string) [3]int {
		var out [3]int
		for i, p := range strings.SplitN(s, ".", 4) {
			if i > 2 {
				break
			}
			out[i], _ = strconv.Atoi(p)
		}
		return out
	}
	a, b := parse(v), parse(min)
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

// instanceConfig is a hosted node's meshtasticd config: a sim radio and nothing else. UDP
// multicast and MQTT stay off (the air bridge and RepeaterTastic's own links carry those).
const instanceConfig = `# written by RepeaterTastic: a hosted node on a simulated radio
Lora:
  Module: sim
Logging:
  LogLevel: info
General:
  MaxNodes: 200
  MaxMessageQueue: 100
`

// instanceConfigFor is the instance's config file: the sim radio, and an I²C bus as well for a node
// that carries sensors. The bus doesn't exist in the kernel and must not: the shim answers the node's
// opens of it before they get there (shim/README.md).
func instanceConfigFor(s *SensorSetup) string {
	if s == nil || s.Device == "" {
		return instanceConfig
	}
	return instanceConfig + "I2C:\n  I2CDevice: " + s.Device + "\n"
}

// Hosted is a meshtasticd RepeaterTastic runs, standing in for one of a host's identities.
type Hosted struct {
	*Node
	inst     Instance
	launcher Launcher
	ctx      context.Context // ends when the instance is stopped
	stop     context.CancelFunc
	done     chan struct{} // closed when the process has stopped for good

	mu       sync.Mutex
	restarts int
	reboots  int
	lastErr  string
	running  bool
	started  time.Time
	stops    []HostedStop
	logTail  []LogLine
	// env and sensors are what the next start of the process gets: SetSensors changes them for a
	// node whose sensors have changed, and the supervisor picks them up when it starts it again.
	env     []string
	sensors *SensorSetup
	// endRun stops the running process without ending the instance (Bounce); bounce says the stop
	// that follows was asked for, not a failure.
	endRun context.CancelFunc
	bounce bool
	// airAtBoot is whether air quality telemetry was on when the node came up on this run, and
	// bootRead whether we have read the node since it started. airArmed stops us restarting it for
	// its air quality setting more than once (see checkSensorBoot).
	airAtBoot bool
	bootRead  bool
	airArmed  bool
}

// HostedStop is one time meshtasticd stopped.
type HostedStop struct {
	Time   int64  `json:"time"` // Unix ms
	Reason string `json:"reason"`
	// Reboot: it stopped to apply settings RepeaterTastic had just given it.
	Reboot bool `json:"reboot"`
}

// LogLine is a line meshtasticd printed.
type LogLine struct {
	Time int64  `json:"time"` // Unix ms
	Text string `json:"text"`
}

const (
	logKeep  = 1000
	stopKeep = 20
	// rebootWindow: a stop this soon after a settings commit is the reboot that applies them.
	rebootWindow = 30 * time.Second
)

// StartHosted writes the instance's config, starts meshtasticd under supervision and connects
// its client. The process restarts (with backoff) until ctx ends.
func StartHosted(ctx context.Context, l Launcher, in Instance, logf func(string, ...any)) (*Hosted, error) {
	if logf == nil {
		logf = discardLogf
	}
	if err := os.MkdirAll(filepath.Join(in.Dir, "vfs"), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(in.ConfigPath(), []byte(instanceConfigFor(in.Sensors)), 0o600); err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(ctx)
	addr := fmt.Sprintf("127.0.0.1:%d", in.Port)
	c := mtclient.New(mtclient.Options{Address: addr, Logf: discardLogf, ReconnectInterval: 500 * time.Millisecond,
		ConfigTimeout: 30 * time.Second})
	h := &Hosted{Node: newNode(addr, in.Dir, c, logf), inst: in, launcher: l, ctx: ctx, stop: stop, done: make(chan struct{})}
	// The sensors and the environment live behind the lock from here on, so a restart can change
	// them; inst keeps only what never changes.
	h.env, h.sensors = in.Env, in.Sensors
	h.inst.Env, h.inst.Sensors = nil, nil
	h.SetAfterConfigured(h.checkSensorBoot)
	go func() {
		defer close(h.done)
		h.supervise(ctx)
	}()
	if err := c.Start(ctx); err != nil {
		stop()
		return nil, err
	}
	return h, nil
}

// Instance describes the hosted node.
func (h *Hosted) Instance() Instance { return h.inst }

// HostedStatus is what the GUI shows about a hosted node.
type HostedStatus struct {
	Name      string `json:"name"`
	Launcher  string `json:"launcher"`
	Port      int    `json:"port"`
	Running   bool   `json:"running"`
	Connected bool   `json:"connected"`
	// Since is when the current process started (Unix ms; 0 while it isn't running).
	Since int64 `json:"since"`
	// Restarts counts unexpected stops; Reboots the ones that applied settings.
	Restarts  int          `json:"restarts"`
	Reboots   int          `json:"reboots"`
	LastError string       `json:"last_error,omitempty"`
	Stops     []HostedStop `json:"stops"`
	Firmware  string       `json:"firmware,omitempty"`
	NodeID    string       `json:"node_id,omitempty"`
	// Sensors are the IDs of the host sensors this node publishes as its own (docs/sensors.md).
	Sensors []string `json:"sensors,omitempty"`
}

// Status reports the process and link state.
func (h *Hosted) Status() HostedStatus {
	s := h.client.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	st := HostedStatus{Name: h.inst.Name, Launcher: h.launcher.Describe(), Port: h.inst.Port, Running: h.running,
		Connected: s.Connected, Restarts: h.restarts, Reboots: h.reboots, LastError: h.lastErr,
		Stops: append([]HostedStop{}, h.stops...), Firmware: s.Metadata.GetFirmwareVersion(),
		Sensors: h.sensors.Sources()}
	if h.running {
		st.Since = h.started.UnixMilli()
	}
	if s.NodeNum() != 0 {
		st.NodeID = fmt.Sprintf("!%08x", s.NodeNum())
	}
	return st
}

// Log is the last lines meshtasticd printed, oldest first.
func (h *Hosted) Log() []LogLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]LogLine{}, h.logTail...)
}

func (h *Hosted) supervise(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		pr, pw := io.Pipe()
		go h.collect(pr)
		// Each run has its own context, so Bounce can stop this process without ending the instance.
		rctx, endRun := context.WithCancel(ctx)
		start := time.Now()
		in := h.startRun(start, endRun)
		err := h.launcher.Run(rctx, in, pw)
		endRun()
		_ = pw.Close()
		if ctx.Err() != nil {
			h.setStopped()
			return
		}
		if err == nil {
			err = errors.New("exited")
		}
		bounced := h.tookBounce()
		if bounced {
			err = errors.New("restarted to look for its sensors")
		}
		// meshtasticd reboots (exits) to apply some settings: that's expected, not a failure. So is
		// a restart we asked for.
		reboot := bounced || time.Since(time.UnixMilli(h.committed.Load())) < rebootWindow
		h.recordStop(err, reboot)
		switch {
		case bounced:
			h.logf("meshtasticd %s (%s) restarts to look for its sensors", h.inst.Name, h.launcher.Describe())
			backoff = time.Second
		case reboot:
			h.logf("meshtasticd %s (%s) rebooted to apply its settings", h.inst.Name, h.launcher.Describe())
			backoff = time.Second
		default:
			h.logf("meshtasticd %s (%s) stopped: %v; restarting in %v", h.inst.Name, h.launcher.Describe(), err, backoff)
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if !reboot {
			backoff = min(backoff*2, time.Minute)
		}
	}
}

// startRun notes the process is starting and returns the instance to run with. The sensors and the
// environment come from here, not from inst, so each start uses the ones the node has now.
func (h *Hosted) startRun(at time.Time, end context.CancelFunc) Instance {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running, h.started, h.endRun, h.bootRead = true, at, end, false
	in := h.inst
	in.Env, in.Sensors = h.env, h.sensors
	return in
}

// tookBounce reports whether the stop that just happened was one Bounce asked for, and forgets it.
func (h *Hosted) tookBounce() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.bounce
	h.bounce = false
	return b
}

// Bounce stops the process; the supervisor starts it again straight away, with whatever SetSensors
// has since put in the instance directory. meshtasticd scans the I²C bus once, at start-up, so a
// sensor attached to a running node is only found after this. The client, the identity and the
// node's state directory are untouched, and no other node is affected.
func (h *Hosted) Bounce() {
	h.mu.Lock()
	end := h.endRun
	// Nothing running yet: its first start already has the current setup.
	h.bounce = end != nil
	h.mu.Unlock()
	if end != nil {
		end()
	}
}

// checkSensorBoot restarts a node carrying a particulate sensor once, if it started without the air
// quality setting the sensor needs. The firmware's air quality module attaches its sensors only when
// it is already enabled as the node scans the bus at start-up, and the setting reaches the node from
// here after it has booted (shim/README.md). Most changes make meshtasticd reboot by itself, which
// is restart enough; this covers the times it doesn't.
func (h *Hosted) checkSensorBoot(s mtclient.Snapshot) {
	on := s.ModuleConfig.GetTelemetry().GetAirQualityEnabled()
	h.mu.Lock()
	if !h.bootRead {
		h.bootRead, h.airAtBoot = true, on
	}
	need := h.sensors.AirQuality() && on && !h.airAtBoot && !h.airArmed
	if need {
		h.airArmed = true
	}
	h.mu.Unlock()
	if !need {
		return
	}
	h.logf("meshtasticd %s: restarting so it finds its air quality sensor (the setting reached it after it had started)", h.inst.Name)
	h.Bounce()
}

// Sensors are the imitated sensors the node carries (nil for a node with none).
func (h *Hosted) Sensors() *SensorSetup {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sensors
}

// SetSensors gives the node the sensors its next start will find, and rewrites its config file so
// meshtasticd looks for the bus (or stops looking). Call Bounce to have it scan for them.
func (h *Hosted) SetSensors(env []string, s *SensorSetup) error {
	h.mu.Lock()
	h.env, h.sensors = env, s
	h.mu.Unlock()
	return os.WriteFile(h.inst.ConfigPath(), []byte(instanceConfigFor(s)), 0o600)
}

func (h *Hosted) setStopped() {
	h.mu.Lock()
	h.running = false
	h.mu.Unlock()
}

// recordStop notes why the process stopped.
func (h *Hosted) recordStop(err error, reboot bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if reboot {
		h.reboots++
	} else {
		h.restarts++
		h.lastErr = err.Error()
	}
	h.stops = append(h.stops, HostedStop{Time: time.Now().UnixMilli(), Reason: err.Error(), Reboot: reboot})
	if len(h.stops) > stopKeep {
		h.stops = h.stops[len(h.stops)-stopKeep:]
	}
}

// collect keeps the last lines meshtasticd printed, for the GUI and for errors.
func (h *Hosted) collect(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		line := sc.Text()
		h.mu.Lock()
		h.logTail = append(h.logTail, LogLine{Time: time.Now().UnixMilli(), Text: line})
		if len(h.logTail) > logKeep {
			h.logTail = h.logTail[len(h.logTail)-logKeep:]
		}
		h.mu.Unlock()
		if (strings.Contains(line, "ERROR") || strings.Contains(line, "CRIT")) && !bootNoise(line) {
			h.logf("meshtasticd %s: %s", h.inst.Name, line)
		}
	}
}

// Close stops the client; the process stops with the context StartHosted was given.
func (h *Hosted) Close() error { return h.client.Close() }

// Context ends when the instance stops.
func (h *Hosted) Context() context.Context { return h.ctx }

// Stop stops the process and the client and waits for the process to end.
func (h *Hosted) Stop() {
	h.stop()
	_ = h.client.Close()
	<-h.done
}

var _ mesh.ConfigApplier = (*Hosted)(nil)

// bootNoise is an error line every fresh or radio-less meshtasticd prints while it starts.
func bootNoise(line string) bool {
	for _, s := range []string{"Can't open/read /prefs/", "No radio instance available to provide entropy",
		"Invalid channel index 0 > 0"} {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

// LauncherFor picks the launcher for the hosted settings: Docker when an image is given, else the
// binary ("" = meshtasticd on PATH).
func LauncherFor(binary, image string) Launcher {
	if image != "" {
		return DockerLauncher{Image: image}
	}
	return ExecLauncher{Binary: binary}
}

// CheckLauncher reports the meshtasticd version l runs, or why it can't host nodes.
func CheckLauncher(ctx context.Context, l Launcher) (string, error) {
	v, err := l.Version(ctx)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
		return "", fmt.Errorf("there's no meshtasticd at %s", l.Describe())
	}
	if err != nil {
		return "", fmt.Errorf("%s can't run: %w", l.Describe(), err)
	}
	if !VersionAtLeast(v, MinFirmware) {
		return v, fmt.Errorf("meshtasticd %s is too old: hosted nodes need %s or newer", v, MinFirmware)
	}
	return v, nil
}

// OfficialImage reports whether image is a meshtasticd image from the Meshtastic project.
func OfficialImage(image string) bool {
	return strings.HasPrefix(image, "meshtastic/meshtasticd:") || strings.HasPrefix(image, "docker.io/meshtastic/meshtasticd:")
}

// MeshtasticdBinary reports whether path names a meshtasticd program ("" = the one on PATH).
func MeshtasticdBinary(path string) bool {
	return path == "" || filepath.Base(path) == "meshtasticd"
}
