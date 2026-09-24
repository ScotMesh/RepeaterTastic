package nodes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/ScotMesh/RepeaterTastic/api/meshtastic"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/mtclient/mtclienttest"
	"github.com/ScotMesh/RepeaterTastic/internal/radio/null"
	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
)

// fakeSensors is a SensorSource for the tests: attachments by short name or node ID, readings per
// source, and a subscription that reports a source as it changes.
type fakeSensors struct {
	mu     sync.Mutex
	attach map[string][]sensors.Attachment
	latest map[string]sensors.Reading
	every  time.Duration
	subs   []chan string
}

func newFakeSensors() *fakeSensors {
	return &fakeSensors{attach: map[string][]sensors.Attachment{}, latest: map[string]sensors.Reading{},
		every: time.Hour}
}

func (f *fakeSensors) For(shortName, nodeID string) []sensors.Attachment {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.attach[strings.ToUpper(shortName)]; ok && shortName != "" {
		return a
	}
	return f.attach[nodeID]
}

func (f *fakeSensors) Latest(id string) (sensors.Reading, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rd, ok := f.latest[id]
	return rd, ok
}

func (f *fakeSensors) Subscribe(buf int) (<-chan string, func()) {
	ch := make(chan string, buf)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	return ch, func() {}
}

func (f *fakeSensors) Interval() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.every
}

// read records a reading and tells the subscribers, as the registry does.
func (f *fakeSensors) read(id string, fields map[sensors.Field]float64) {
	f.mu.Lock()
	f.latest[id] = sensors.Reading{At: time.Now(), Fields: fields}
	subs := append([]chan string{}, f.subs...)
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- id:
		default:
		}
	}
}

// fakeShim stands in for a build made with `make shim`, so the tests don't need a real library.
func fakeShim(t *testing.T) []byte {
	t.Helper()
	lib := []byte("\x7fELF not really a shim")
	prev := shimLibrary
	shimLibrary = func() ([]byte, error) { return lib, nil }
	t.Cleanup(func() { shimLibrary = prev })
	return lib
}

// TestPlanSensorsChips plans the chips from the attachments' fields, and drops a field no chip can
// carry rather than failing.
func TestPlanSensorsChips(t *testing.T) {
	src := newFakeSensors()
	src.read("weather", map[sensors.Field]float64{sensors.Temperature: 11})
	cases := []struct {
		name       string
		atts       []sensors.Attachment
		wantChips  string
		wantFields string
		wantAir    bool
	}{
		{"one field, one chip", []sensors.Attachment{{Sensor: "shed", Fields: []sensors.Field{sensors.Temperature}}},
			"pct2075", "temperature", false},
		{"two sources, three chips", []sensors.Attachment{
			{Sensor: "shed", Fields: []sensors.Field{sensors.Humidity, sensors.Temperature}},
			{Sensor: "rack", Fields: []sensors.Field{sensors.Voltage, sensors.Current}}},
			"aht10,ina226,pct2075", "current,humidity,temperature,voltage", false},
		{"one chip for both particulate fields", []sensors.Attachment{
			{Sensor: "air", Fields: []sensors.Field{sensors.PM25, sensors.PM100}}}, "pmsa003i", "pm100,pm25", true},
		{"no fields: whatever the source reports", []sensors.Attachment{{Sensor: "weather"}}, "pct2075", "temperature", false},
		{"a field no chip carries is dropped", []sensors.Attachment{
			{Sensor: "shed", Fields: []sensors.Field{sensors.Pressure, sensors.Temperature}}}, "pct2075", "temperature", false},
		{"nothing carried at all", []sensors.Attachment{
			{Sensor: "shed", Fields: []sensors.Field{sensors.Pressure}}}, "", "", false},
		{"nothing attached", nil, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := planSensors(src, tc.atts, nil)
			if tc.wantChips == "" {
				if p != nil {
					t.Fatalf("plan = %+v, want none", p)
				}
				return
			}
			if p == nil {
				t.Fatal("no plan")
			}
			if got := chipSet(p.Chips); got != tc.wantChips {
				t.Errorf("chips %q, want %q", got, tc.wantChips)
			}
			if got := joinFields(p.Fields); got != tc.wantFields {
				t.Errorf("fields %q, want %q", got, tc.wantFields)
			}
			// Particulates reach the air through the air quality module, not the environment one.
			if p.AirQuality() != tc.wantAir {
				t.Errorf("air quality = %v", p.AirQuality())
			}
		})
	}
}

// chipSet is a plan's chips by name, sorted, so a test doesn't depend on the order they are planned
// in (that belongs to internal/sensors).
func chipSet(chips []sensors.Chip) string {
	names := strings.Split(sensors.ChipNames(chips), ",")
	sort.Strings(names)
	return strings.Join(names, ",")
}

func joinFields(f []sensors.Field) string {
	out := make([]string, len(f))
	for i, v := range f {
		out[i] = string(v)
	}
	return strings.Join(out, ",")
}

// TestSeedSensorsExecLauncher: a meshtasticd binary sees the real paths, and the values file is
// there with every planned field before anything starts.
func TestSeedSensorsExecLauncher(t *testing.T) {
	lib := fakeShim(t)
	src := newFakeSensors()
	src.read("shed", map[sensors.Field]float64{sensors.Temperature: 18.4})
	in := Instance{Name: "main-abc", Dir: filepath.Join(t.TempDir(), "abc"), Port: 4501, HWID: "02A5DEADBEEF"}
	p := planSensors(src, []sensors.Attachment{{Sensor: "shed",
		Fields: []sensors.Field{sensors.Temperature, sensors.Humidity}}}, nil)
	if err := seedSensors(ExecLauncher{}, &in, src, p); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"LD_PRELOAD=" + filepath.Join(in.Dir, "i2cshim.so"),
		"I2CSHIM_DEV=/dev/i2c-fake0",
		"I2CSHIM_VALUES=" + filepath.Join(in.Dir, "sensors.values"),
		"I2CSHIM_CHIPS=" + sensors.ChipNames(p.Chips),
		"I2CSHIM_LINEBUF=1",
	}
	if strings.Join(in.Env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env\n%s\nwant\n%s", strings.Join(in.Env, "\n"), strings.Join(want, "\n"))
	}
	// The shim is unpacked ready to preload, and a field with no reading yet reads 0 so the file can
	// answer the node's start-up scan.
	if b, err := os.ReadFile(filepath.Join(in.Dir, "i2cshim.so")); err != nil || string(b) != string(lib) {
		t.Fatalf("shim library: %v", err)
	}
	st, err := os.Stat(filepath.Join(in.Dir, "i2cshim.so"))
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("shim mode %v: %v", st.Mode(), err)
	}
	if got := chipSet(p.Chips); got != "aht10,pct2075" {
		t.Fatalf("chips %q", got)
	}
	if got := readFile(t, valuesPath(in.Dir)); got != "humidity=0.0000\ntemperature=18.4000" {
		t.Fatalf("values %q", got)
	}
	// The config file tells meshtasticd where the bus is; only a node with sensors gets one.
	if got := instanceConfigFor(in.Sensors); !strings.HasSuffix(got, "I2C:\n  I2CDevice: /dev/i2c-fake0\n") {
		t.Fatalf("config\n%s", got)
	}
	if instanceConfigFor(nil) != instanceConfig || strings.Contains(instanceConfig, "I2C") {
		t.Fatal("a node with no sensors must get no I2C section")
	}
}

// TestSeedSensorsDockerLauncher: in a container every path is the one under /data, where the
// instance directory is mounted.
func TestSeedSensorsDockerLauncher(t *testing.T) {
	fakeShim(t)
	src := newFakeSensors()
	in := Instance{Name: "main-abc", Dir: t.TempDir(), Port: 4501}
	p := planSensors(src, []sensors.Attachment{{Sensor: "rack",
		Fields: []sensors.Field{sensors.Voltage, sensors.Current}}}, nil)
	if err := seedSensors(DockerLauncher{Image: "meshtastic/meshtasticd:test"}, &in, src, p); err != nil {
		t.Fatal(err)
	}
	want := []string{"LD_PRELOAD=/data/i2cshim.so", "I2CSHIM_DEV=/dev/i2c-fake0",
		"I2CSHIM_VALUES=/data/sensors.values", "I2CSHIM_CHIPS=ina226", "I2CSHIM_LINEBUF=1"}
	if strings.Join(in.Env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env %q, want %q", in.Env, want)
	}
	if !strings.Contains(instanceConfigFor(in.Sensors), "I2CDevice: /dev/i2c-fake0") {
		t.Fatalf("config\n%s", instanceConfigFor(in.Sensors))
	}
	// The file itself is written on the host, in the directory Docker mounts.
	if got := readFile(t, valuesPath(in.Dir)); got != "current=0.0000\nvoltage=0.0000" {
		t.Fatalf("values %q", got)
	}
}

// The container gets the environment as -e arguments; the instance directory is already mounted, so
// nothing else changes.
func TestDockerLauncherPassesSensorEnv(t *testing.T) {
	dir := fakeBin(t)
	in := testInstance(t)
	in.Env = []string{"LD_PRELOAD=/data/i2cshim.so", "I2CSHIM_CHIPS=pct2075"}
	var out syncBuffer
	if err := (DockerLauncher{Image: "img"}).Run(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	run := readFile(t, filepath.Join(dir, "docker.args"))
	for _, want := range []string{"-e LD_PRELOAD=/data/i2cshim.so", "-e I2CSHIM_CHIPS=pct2075",
		"-v " + in.Dir + ":/data ", " img /usr/bin/meshtasticd -c /data/config.yaml"} {
		if !strings.Contains(run, want) {
			t.Errorf("docker args %q lack %q", run, want)
		}
	}
}

// A meshtasticd binary is started with the same variables in its environment.
func TestExecLauncherPassesSensorEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "meshtasticd")
	script := "#!/bin/sh\nenv | grep -E '^(LD_PRELOAD|I2CSHIM_)' | sort > \"" + dir + "/env.txt\"\necho booting\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	in := testInstance(t)
	in.Env = []string{"I2CSHIM_VALUES=" + valuesPath(in.Dir), "I2CSHIM_CHIPS=pct2075,aht10"}
	var out syncBuffer
	if err := (ExecLauncher{Binary: bin}).Run(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	want := "I2CSHIM_CHIPS=pct2075,aht10\nI2CSHIM_VALUES=" + valuesPath(in.Dir)
	if got := readFile(t, filepath.Join(dir, "env.txt")); got != want {
		t.Fatalf("environment %q, want %q", got, want)
	}
}

// Without the feature nothing changes: no environment, no bus, no files.
func TestSeedSensorsOffChangesNothing(t *testing.T) {
	in := Instance{Name: "main-abc", Dir: t.TempDir()}
	if err := seedSensors(ExecLauncher{}, &in, nil, nil); err != nil {
		t.Fatal(err)
	}
	if in.Env != nil || in.Sensors != nil {
		t.Fatalf("instance = %+v", in)
	}
	if entries, err := os.ReadDir(in.Dir); err != nil || len(entries) != 0 {
		t.Fatalf("%d files written: %v", len(entries), err)
	}
}

// A build without the library says what to run rather than failing to compile.
func TestSeedSensorsWithoutTheShim(t *testing.T) {
	if HasShim() {
		t.Skip("this build carries the shim (make shim has run)")
	}
	src := newFakeSensors()
	in := Instance{Name: "main-abc", Dir: t.TempDir()}
	p := planSensors(src, []sensors.Attachment{{Sensor: "shed", Fields: []sensors.Field{sensors.Temperature}}}, nil)
	err := seedSensors(ExecLauncher{}, &in, src, p)
	if err == nil || !strings.Contains(err.Error(), "make shim") {
		t.Fatalf("err = %v", err)
	}
}

// The readings file is rewritten when a reading changes and left alone when it doesn't: every source
// is written to every node that publishes it, every time it is read.
func TestValuesFileRewrittenOnlyOnChange(t *testing.T) {
	src := newFakeSensors()
	src.read("shed", map[sensors.Field]float64{sensors.Temperature: 18.4})
	p := planSensors(src, []sensors.Attachment{{Sensor: "shed", Fields: []sensors.Field{sensors.Temperature}}}, nil)
	path := valuesPath(t.TempDir())
	for _, tc := range []struct {
		name        string
		temp        float64
		wantChanged bool
	}{
		{"first write", 18.4, true},
		{"the same reading again", 18.4, false},
		{"a new reading", 18.9, true},
		{"and again unchanged", 18.9, false},
	} {
		src.read("shed", map[sensors.Field]float64{sensors.Temperature: tc.temp})
		changed, err := sensors.WriteValuesFile(path, valuesFor(src, p))
		if err != nil {
			t.Fatal(err)
		}
		if changed != tc.wantChanged {
			t.Errorf("%s: changed = %v", tc.name, changed)
		}
	}
	if got := readFile(t, path); got != "temperature=18.9000" {
		t.Fatalf("values %q", got)
	}
}

// recordingLauncher runs nodes like tcpLauncher and remembers the instance each start was given.
type recordingLauncher struct {
	*tcpLauncher
	mu     sync.Mutex
	last   map[int]Instance
	starts map[int]int
}

func (l *recordingLauncher) Run(ctx context.Context, in Instance, out io.Writer) error {
	l.mu.Lock()
	l.last[in.Port] = in
	l.starts[in.Port]++
	l.mu.Unlock()
	go l.rebootOnCommit(ctx, in.Port)
	return l.tcpLauncher.Run(ctx, in, out)
}

// rebootOnCommit makes the node on this port drop the connection to apply a settings change, as a
// real meshtasticd does, as soon as the launcher has created it.
func (l *recordingLauncher) rebootOnCommit(ctx context.Context, port int) {
	for ctx.Err() == nil {
		if n := l.node(port); n != nil {
			n.SetRebootOnCommit(true)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (l *recordingLauncher) instance(port int) Instance {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last[port]
}

func (l *recordingLauncher) started(port int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts[port]
}

// TestHostingSensors hosts two identities, one of which publishes a sensor, and checks the node gets
// its bus, its file and its telemetry setting while the other is left plain.
func TestHostingSensors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fakeShim(t)
	h, err := mesh.NewHost(mesh.Config{Region: "EU_868", Preset: pb.Config_LoRaConfig_LONG_FAST,
		RelayRole: mesh.RoleClientMute, StateDir: t.TempDir()}, null.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	src := newFakeSensors()
	src.attach["DESK"] = []sensors.Attachment{{Sensor: "shed",
		Fields: []sensors.Field{sensors.Temperature, sensors.Humidity}}}
	src.read("shed", map[sensors.Field]float64{sensors.Temperature: 18.4, sensors.Humidity: 63.2})
	src.every = time.Minute // below the firmware's floor: the node is asked for the floor

	base := freePortBase(t)
	l := &recordingLauncher{tcpLauncher: &tcpLauncher{nodes: map[int]*mtclienttest.Node{}},
		last: map[int]Instance{}, starts: map[int]int{}}
	x := NewHosting(ctx, HostingOptions{Launcher: l, Air: NewLoRaAir(h, testLogf(t)), Radio: "main", Dir: t.TempDir(),
		PortBase: base, Logf: testLogf(t), Sensors: src,
		RelayOwner: func() (string, string) { return "RT Relay", "RTR" }})
	h.SetHoster(x)

	newID := distinctIdentities()
	relay := newID("Relay", "RLY")
	relay.IsRelay = true
	desk := newID("Desk", "DESK")
	addRecords(ctx, t, h, relay, desk)
	var node *mtclienttest.Node
	eventually(t, "both nodes started", func() bool {
		node = l.node(base + 1)
		return node != nil && l.node(base) != nil
	})

	// The identity that publishes the sensor carries it; the persona carries nothing.
	in := l.instance(base + 1)
	if in.Sensors == nil || chipSet(in.Sensors.Chips) != "aht10,pct2075" {
		t.Fatalf("identity's sensors = %+v", in.Sensors)
	}
	if got := strings.Join(in.Env, " "); !strings.Contains(got, "I2CSHIM_CHIPS="+sensors.ChipNames(in.Sensors.Chips)) ||
		!strings.Contains(got, "LD_PRELOAD="+filepath.Join(in.Dir, "i2cshim.so")) {
		t.Fatalf("identity's environment %q", got)
	}
	if p := l.instance(base).Sensors; p != nil {
		t.Fatalf("the persona carries %+v", p)
	}
	if got := readFile(t, l.instance(base).ConfigPath()); strings.Contains(got, "I2C") {
		t.Fatalf("the persona's config has a bus:\n%s", got)
	}
	// The file and the bus were there before meshtasticd started: it scans once, at start-up.
	if got := readFile(t, in.ConfigPath()); !strings.Contains(got, "I2CDevice: /dev/i2c-fake0") {
		t.Fatalf("identity's config\n%s", got)
	}
	if got := readFile(t, valuesPath(in.Dir)); got != "humidity=63.2000\ntemperature=18.4000" {
		t.Fatalf("values %q", got)
	}

	// The GUI can see what a node carries, and the node is asked to broadcast it.
	eventually(t, "sensors in the status", func() bool {
		for _, n := range x.Nodes() {
			if n.Port == base+1 {
				return len(n.Sensors) == 1 && n.Sensors[0] == "shed"
			}
		}
		return false
	})
	eventually(t, "environment telemetry on", func() bool {
		tel := node.Modules().Telemetry
		return tel.GetEnvironmentMeasurementEnabled() && tel.GetEnvironmentUpdateInterval() == uint32(MinEnvInterval/time.Second)
	})
	if tel := l.node(base).Modules().Telemetry; tel.GetEnvironmentMeasurementEnabled() || tel.GetAirQualityEnabled() {
		t.Fatal("the persona has no sensors: its telemetry must stay off")
	}
	if tel := node.Modules().Telemetry; tel.GetAirQualityEnabled() {
		t.Fatal("no particulates here: air quality telemetry must stay off")
	}

	// A new reading rewrites the file, with no restart and no packet from here.
	src.read("shed", map[sensors.Field]float64{sensors.Temperature: 19.6, sensors.Humidity: 63.2})
	eventually(t, "the new reading reaches the node's file", func() bool {
		b, err := os.ReadFile(valuesPath(in.Dir))
		return err == nil && strings.Contains(string(b), "temperature=19.6000")
	})
	if l.started(base+1) != 1 || l.started(base) != 1 {
		t.Fatalf("a reading restarted a node: %d, %d", l.started(base), l.started(base+1))
	}
	// The same reading again leaves the file alone, so the node's directory isn't churned.
	before, err := os.Stat(valuesPath(in.Dir))
	if err != nil {
		t.Fatal(err)
	}
	src.read("shed", map[sensors.Field]float64{sensors.Temperature: 19.6, sensors.Humidity: 63.2})
	time.Sleep(50 * time.Millisecond)
	after, err := os.Stat(valuesPath(in.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged reading rewrote the file")
	}

	// A sensor attached to a running identity needs a restart: only that node's process stops.
	src.attach["DESK"] = append(src.attach["DESK"],
		sensors.Attachment{Sensor: "rack", Fields: []sensors.Field{sensors.Voltage}},
		sensors.Attachment{Sensor: "street", Fields: []sensors.Field{sensors.PM25}})
	src.read("rack", map[sensors.Field]float64{sensors.Voltage: 12.1})
	src.read("street", map[sensors.Field]float64{sensors.PM25: 7})
	if err := x.Restart(desk.NodeID()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the identity's node starts again", func() bool { return l.started(base+1) >= 2 })
	if l.started(base) != 1 {
		t.Fatalf("the persona restarted too (%d starts)", l.started(base))
	}
	if got := chipSet(l.instance(base + 1).Sensors.Chips); got != "aht10,ina226,pct2075,pmsa003i" {
		t.Fatalf("chips after the restart %q", got)
	}
	if got := readFile(t, valuesPath(in.Dir)); !strings.Contains(got, "voltage=12.1000") ||
		!strings.Contains(got, "pm25=7.0000") {
		t.Fatalf("values after the restart %q", got)
	}
	// Particulates go out through the air quality module, which has to be on before the node scans
	// its bus. It boots without it, is given it, and starts once more by itself to find the sensor.
	eventually(t, "air quality telemetry on", func() bool {
		tel := node.Modules().Telemetry
		return tel.GetAirQualityEnabled() && tel.GetEnvironmentMeasurementEnabled() &&
			tel.GetAirQualityInterval() == uint32(MinEnvInterval/time.Second)
	})
	eventually(t, "it starts again with the air quality setting in place", func() bool { return l.started(base+1) == 3 })
	// And exactly once: a node must not restart itself in a loop.
	time.Sleep(300 * time.Millisecond)
	if got := l.started(base + 1); got != 3 {
		t.Fatalf("%d starts: the node keeps restarting itself", got)
	}
	// Every restart here was one we asked for, not a failure.
	if hh := x.Health(); hh.State == HealthWarning || hh.State == HealthError {
		t.Fatalf("health after a restart: %+v", hh)
	}
}

// Restart says which node it can't find rather than restarting something else.
func TestRestartUnknownNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := NewHosting(ctx, HostingOptions{Launcher: &tcpLauncher{nodes: map[int]*mtclienttest.Node{}},
		Radio: "main", Dir: t.TempDir(), PortBase: freePortBase(t), Logf: testLogf(t)})
	for _, id := range []string{"!a1c40e07", "a1c40e07"} {
		err := x.Restart(id)
		if err == nil || !strings.Contains(err.Error(), "!a1c40e07") || !strings.Contains(err.Error(), "radio main") {
			t.Fatalf("Restart(%q) = %v", id, err)
		}
	}
	if err := x.Restart(" "); err == nil || !strings.Contains(err.Error(), "which identity") {
		t.Fatalf("Restart(\"\") = %v", err)
	}
}

// A build with no shim for this architecture, or an instance directory that can't be written, must
// not keep an identity off the mesh: it comes up without its sensors and says why.
func TestIdentityStartsWhenItsSensorsCannot(t *testing.T) {
	prev := shimLibrary
	shimLibrary = func() ([]byte, error) { return nil, errors.New("no I²C shim for linux/mips") }
	t.Cleanup(func() { shimLibrary = prev })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := mesh.NewHost(mesh.Config{Region: "EU_868", Preset: pb.Config_LoRaConfig_LONG_FAST, StateDir: t.TempDir()},
		null.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	src := newFakeSensors()
	src.read("shed", map[sensors.Field]float64{sensors.Temperature: 19})
	src.attach["SHED"] = []sensors.Attachment{{Sensor: "shed", Fields: []sensors.Field{sensors.Temperature}}}

	// The node logs from its own goroutine as well as ours, so the sink is locked.
	var logMu sync.Mutex
	var logged []string
	x := NewHosting(ctx, HostingOptions{Launcher: &fakeLauncher{}, Air: NewLoRaAir(h, nil), Radio: "main",
		Dir: t.TempDir(), PortBase: 45700, Sensors: src,
		Logf: func(f string, a ...any) {
			logMu.Lock()
			logged = append(logged, fmt.Sprintf(f, a...))
			logMu.Unlock()
		}})

	shed, _ := mesh.NewIdentity(nil, "Shed Watch", "SHED")
	id, err := x.HostIdentity(ctx, h, shed.Record())
	if err != nil || id == nil {
		t.Fatalf("the identity stayed off air over a sensor: %v", err)
	}
	nodes := x.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("hosted nodes = %d, want the identity running", len(nodes))
	}
	if got := nodes[0].Sensors; len(got) != 0 {
		t.Errorf("node carries %v, want nothing: the shim could not be unpacked", got)
	}
	logMu.Lock()
	lines := strings.Join(logged, "\n")
	logMu.Unlock()
	if !strings.Contains(lines, "starting this node without them") {
		t.Errorf("logged %q, want a line saying the node came up without its sensors", lines)
	}
}
