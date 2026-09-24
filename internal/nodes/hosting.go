package nodes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
	"github.com/ScotMesh/RepeaterTastic/internal/wire"
)

// SlotsPerRadio is the size of each radio's block of client API ports: the relay persona takes
// the first, identities the rest.
const SlotsPerRadio = 100

// HostingOptions say how a radio's identities run on meshtasticd.
type HostingOptions struct {
	Launcher Launcher
	Air      Air
	Radio    string // radio ID: names the instances
	Dir      string // instance state directories live here
	PortBase int    // the radio's first client API port (the persona's)
	// HopsBehind is how far joined nodes are from the air (1 behind a board's relay).
	HopsBehind uint32
	// RelayOwner names the persona (the configured relay names).
	RelayOwner func() (long, short string)
	Logf       func(string, ...any)
	// NodeLogf, when set, logs for one node (by node ID), so its lines say which identity they're about.
	NodeLogf func(nodeID string) func(string, ...any)
	// Sensors, when set, offers each identity the host sensors it publishes as its own: the node is
	// given an imitated I²C bus and the readings file its shim answers from (sensors.go). nil means
	// the feature is off and no node carries anything.
	Sensors SensorSource
}

// Hosting runs a radio's identities as hosted meshtasticd nodes on its air: a mesh.Hoster.
type Hosting struct {
	ctx  context.Context // instances live as long as this
	opts HostingOptions

	mu        sync.Mutex
	nodes     map[*Node]*hostedEntry
	starting  map[int]bool // port slots held by starts in progress
	version   string       // meshtasticd's version, once checked
	launchErr string       // why meshtasticd can't run, from the last check
	// valuesQuiet remembers, per node, that its last readings-file write failed, so the same
	// failure is logged once rather than at every reading.
	valuesQuiet map[string]bool
}

type hostedEntry struct {
	hn      *Hosted
	host    *mesh.Host
	slot    int
	role    string
	started time.Time
	running chan struct{} // closed when the node's event loop has ended
}

var _ mesh.Hoster = (*Hosting)(nil)

// NewHosting runs hosted nodes until ctx ends.
func NewHosting(ctx context.Context, o HostingOptions) *Hosting {
	if o.Logf == nil {
		o.Logf = discardLogf
	}
	x := &Hosting{ctx: ctx, opts: o, nodes: map[*Node]*hostedEntry{}, starting: map[int]bool{}}
	if o.Sensors != nil {
		go x.followSensors(ctx)
	}
	return x
}

// HostIdentity starts a meshtasticd for rec, seeded with its key, and adds its identity to h.
func (x *Hosting) HostIdentity(ctx context.Context, h *mesh.Host, rec mesh.IdentityRecord) (*mesh.Identity, error) {
	if rec.IsRelay && x.opts.Air.Relay() != nil {
		return nil, errors.New("the radio's board is its relay")
	}
	num := rec.NodeNum()
	if num == 0 {
		return nil, fmt.Errorf("bad key for %q", rec.LongName)
	}
	x.mu.Lock()
	slot, role := 0, "persona"
	if !rec.IsRelay {
		role = "identity"
		if slot = x.freeSlot(); slot == 0 {
			x.mu.Unlock()
			return nil, fmt.Errorf("no free port: a radio hosts at most %d identities", SlotsPerRadio-1)
		}
	}
	x.starting[slot] = true
	x.mu.Unlock()
	defer func() {
		x.mu.Lock()
		delete(x.starting, slot)
		x.mu.Unlock()
	}()

	nodeID := strings.TrimPrefix(wire.NodeID(num), "!")
	in := Instance{Name: "persona-" + x.opts.Radio, Dir: filepath.Join(x.opts.Dir, "persona"),
		Port: x.opts.PortBase, HWID: HWIDFor(x.opts.Radio + "/persona")}
	if !rec.IsRelay {
		in = Instance{Name: x.opts.Radio + "-" + nodeID, Dir: filepath.Join(x.opts.Dir, nodeID),
			Port: x.opts.PortBase + slot, HWID: HWIDFor(x.opts.Radio + "/" + nodeID)}
	}
	// The node lives as long as the hosting (x.ctx), not the request that started it.
	logf := x.opts.Logf
	if x.opts.NodeLogf != nil {
		logf = x.opts.NodeLogf(wire.NodeID(num))
	}
	plan := x.planFor(rec.ShortName, wire.NodeID(num), logf)
	if err := seedSensors(x.opts.Launcher, &in, x.opts.Sensors, plan); err != nil {
		return nil, err
	}
	hn, err := StartHosted(x.ctx, x.opts.Launcher, in, logf)
	if err != nil {
		return nil, err
	}
	if x.opts.Sensors != nil {
		hn.SetSensorTelemetry(plan != nil, plan.AirQuality(), x.envInterval())
	}
	if rec.IsRelay && x.opts.RelayOwner != nil {
		hn.SetOwner(x.opts.RelayOwner())
	}
	hn.SetSeed(rec)
	hn.SetHopsBehind(x.opts.HopsBehind)
	id, err := hn.Identity(ctx, 0)
	if err == nil {
		err = h.AddIdentity(id)
	}
	if err != nil {
		hn.Stop()
		return nil, err
	}
	h.AddConfigApplier(hn)
	hn.Bind(h, id)
	done := make(chan struct{})
	go func() {
		defer close(done)
		hn.Run(hn.Context())
	}()
	x.opts.Air.Join(hn.Context(), hn.Node)
	x.mu.Lock()
	x.nodes[hn.Node] = &hostedEntry{hn: hn, host: h, slot: slot, role: role, started: time.Now(), running: done}
	x.mu.Unlock()
	logf("meshtasticd: %s %s runs on %s, port %d", role, id.NodeID(), x.opts.Launcher.Describe(), in.Port)
	if plan != nil {
		logf("meshtasticd: %s %s carries %s as %s", role, id.NodeID(),
			strings.Join(plan.Sources(), ", "), sensors.ChipNames(plan.Chips))
	}
	return id, nil
}

// planFor is what the identity with these names publishes, or nil: nothing attached, or the sensors
// feature off.
func (x *Hosting) planFor(shortName, nodeID string, logf func(string, ...any)) *SensorSetup {
	if x.opts.Sensors == nil {
		return nil
	}
	return planSensors(x.opts.Sensors, x.opts.Sensors.For(shortName, nodeID), logf)
}

// envInterval is how often a node carrying sensors is asked to broadcast them, never more often
// than the firmware's own floor.
func (x *Hosting) envInterval() time.Duration {
	if x.opts.Sensors == nil {
		return 0
	}
	return max(x.opts.Sensors.Interval(), MinEnvInterval)
}

// followSensors rewrites each node's readings file as new readings arrive, until ctx ends. That is
// all it takes: the node reads the file itself on its own schedule, so nothing restarts and
// RepeaterTastic sends no packet.
func (x *Hosting) followSensors(ctx context.Context) {
	ch, stop := x.opts.Sensors.Subscribe(32)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-ch:
			if !ok {
				return
			}
			x.writeValues(id)
		}
	}
}

// writeValues puts the current readings in the file of every node that publishes source id. The
// file is left alone when the bytes don't change, so an unchanged reading doesn't churn a directory
// per identity per minute.
func (x *Hosting) writeValues(id string) {
	x.mu.Lock()
	var hs []*Hosted
	for _, e := range x.nodes {
		if e.hn.Sensors().publishes(id) {
			hs = append(hs, e.hn)
		}
	}
	x.mu.Unlock()
	for _, hn := range hs {
		p := hn.Sensors()
		if p == nil {
			continue // its sensors changed while we looked
		}
		_, err := sensors.WriteValuesFile(valuesPath(hn.Instance().Dir), valuesFor(x.opts.Sensors, p))
		x.noteValuesWrite(hn.Instance().Name, err)
	}
}

// noteValuesWrite logs a node's first failed write and its recovery, not every one: a directory
// that can't be written fails again at every reading, and a line a minute per identity would bury
// the log.
func (x *Hosting) noteValuesWrite(name string, err error) {
	x.mu.Lock()
	if x.valuesQuiet == nil {
		x.valuesQuiet = map[string]bool{}
	}
	quiet := x.valuesQuiet[name]
	x.valuesQuiet[name] = err != nil
	x.mu.Unlock()
	switch {
	case err != nil && !quiet:
		x.opts.Logf("sensors: %s: %v", name, err)
	case err == nil && quiet:
		x.opts.Logf("sensors: %s: its readings file can be written again", name)
	}
}

// Restart stops one identity's meshtasticd and starts it again, so it finds the sensors it now
// carries: meshtasticd scans the I²C bus once, at start-up. Every other node keeps running, and this
// one keeps its key, its settings and its client API port.
func (x *Hosting) Restart(nodeID string) error {
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(nodeID), "!"))
	if want == "" {
		return errors.New("say which identity's node to restart")
	}
	x.mu.Lock()
	var found *hostedEntry
	for _, e := range x.nodes {
		if id := e.hn.Current(); id != nil && strings.TrimPrefix(strings.ToLower(id.NodeID()), "!") == want {
			found = e
			break
		}
	}
	x.mu.Unlock()
	if found == nil {
		return fmt.Errorf("no meshtasticd on radio %s stands for !%s: check the identity is hosted on this radio", x.opts.Radio, want)
	}
	if err := x.reseed(found); err != nil {
		return err
	}
	found.hn.Bounce()
	x.opts.Logf("meshtasticd: %s !%s restarts to look for its sensors", found.role, want)
	return nil
}

// reseed re-plans a node's sensors and writes them into its instance directory, ready for the next
// start. The node keeps running until something stops it (Bounce).
func (x *Hosting) reseed(e *hostedEntry) error {
	if x.opts.Sensors == nil {
		return nil
	}
	short, nodeID := "", ""
	if seed := e.hn.Seed(); seed != nil {
		short = seed.ShortName
	}
	if id := e.hn.Current(); id != nil {
		nodeID = id.NodeID()
		if short == "" {
			short = id.UserCopy().GetShortName()
		}
	}
	in := e.hn.Instance()
	plan := x.planFor(short, nodeID, x.opts.Logf)
	if err := seedSensors(x.opts.Launcher, &in, x.opts.Sensors, plan); err != nil {
		return err
	}
	if err := e.hn.SetSensors(in.Env, in.Sensors); err != nil {
		return err
	}
	e.hn.SetSensorTelemetry(plan != nil, plan.AirQuality(), x.envInterval())
	return nil
}

// freeSlot is the lowest identity port slot not in use, or 0. Called with mu held.
func (x *Hosting) freeSlot() int {
	used := map[int]bool{}
	for s := range x.starting {
		used[s] = true
	}
	for _, e := range x.nodes {
		used[e.slot] = true
	}
	for s := 1; s < SlotsPerRadio; s++ {
		if !used[s] {
			return s
		}
	}
	return 0
}

// Unhost stops the meshtasticd an identity stands for and deletes its state (the identity's key
// and settings are the host's to keep).
func (x *Hosting) Unhost(id *mesh.Identity) {
	n, _ := id.Remote().(*Node)
	x.mu.Lock()
	e := x.nodes[n]
	delete(x.nodes, n)
	x.mu.Unlock()
	if e == nil {
		return
	}
	e.host.RemoveConfigApplier(e.hn)
	x.opts.Air.Leave(e.hn.Node)
	e.hn.Stop()
	<-e.running
	if e.role != "persona" {
		if err := os.RemoveAll(e.hn.Instance().Dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			x.opts.Logf("meshtasticd: removing %s: %v", e.hn.Instance().Dir, err)
		}
	}
	x.opts.Logf("meshtasticd: %s %s stopped", e.role, id.NodeID())
}

// HostedNode is one running instance, for the GUI.
type HostedNode struct {
	Role string // persona or identity
	HostedStatus
}

// Nodes lists the running instances, persona first, then by port.
func (x *Hosting) Nodes() []HostedNode {
	x.mu.Lock()
	var out []HostedNode
	for _, e := range x.nodes {
		out = append(out, HostedNode{Role: e.role, HostedStatus: e.hn.Status()})
	}
	x.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// Launcher runs the instances.
func (x *Hosting) Launcher() Launcher { return x.opts.Launcher }

// SetLauncherCheck records the result of checking the launcher (CheckLauncher).
func (x *Hosting) SetLauncherCheck(version string, err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.version, x.launchErr = version, ""
	if err != nil {
		x.launchErr = err.Error()
	}
}

// Health states, worst last.
const (
	HealthOK       = "ok"
	HealthStarting = "starting"
	HealthWarning  = "warning"
	HealthError    = "error"
)

// startGrace is how long a new node may take to come up before it counts as a problem.
const startGrace = 90 * time.Second

// Health is how a radio's hosted nodes are doing, for the status bar.
type Health struct {
	State    string   `json:"state"` // ok, starting, warning (some identities down) or error (meshtasticd isn't running)
	Nodes    int      `json:"nodes"`
	Up       int      `json:"up"`
	Problems []string `json:"problems,omitempty"`
	Version  string   `json:"version,omitempty"`
	Launcher string   `json:"launcher"`
}

// settling reports whether a node that isn't up is still starting or rebooting, so not yet a problem.
func (e *hostedEntry) settling(st HostedStatus, now time.Time) bool {
	return now.Sub(e.started) < startGrace && st.Restarts == 0 || rebooting(st, now)
}

// downReason says why a node isn't up.
func downReason(st HostedStatus) string {
	switch {
	case st.LastError != "":
		return st.LastError
	case !st.Running:
		return "not running"
	}
	return "not connected"
}

// Health reports whether meshtasticd runs and every node is connected.
func (x *Hosting) Health() Health {
	now := time.Now()
	x.mu.Lock()
	h := Health{State: HealthOK, Version: x.version, Launcher: x.opts.Launcher.Describe()}
	launchErr := x.launchErr
	type row struct {
		e  *hostedEntry
		st HostedStatus
	}
	rows := make([]row, 0, len(x.nodes))
	for _, e := range x.nodes {
		rows = append(rows, row{e, e.hn.Status()})
	}
	x.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].st.Port < rows[j].st.Port })

	h.Nodes = len(rows)
	starting, personaDown := false, false
	for _, r := range rows {
		if r.st.Running && r.st.Connected {
			h.Up++
			if p := r.e.hn.SettingsProblem(); p != "" {
				h.Problems = append(h.Problems, fmt.Sprintf("%s: meshtasticd %s", r.e.who(), p))
			}
			continue
		}
		if r.e.settling(r.st, now) {
			starting = true
			continue
		}
		who := r.e.who()
		if r.e.role == "persona" {
			personaDown = true
		}
		h.Problems = append(h.Problems, fmt.Sprintf("%s: %s", who, downReason(r.st)))
	}
	switch {
	case launchErr != "" && h.Up == 0:
		h.State = HealthError
		h.Problems = append([]string{"meshtasticd can't run: " + launchErr}, h.Problems...)
	case h.Nodes > 0 && h.Up == 0 && !starting:
		h.State = HealthError
	case personaDown:
		h.State = HealthError
	case len(h.Problems) > 0:
		h.State = HealthWarning
	case starting:
		h.State = HealthStarting
	}
	return h
}

// label names the identity a hosted node stands for.
func (h *Hosted) label() string {
	if id := h.Current(); id != nil {
		if u := id.UserCopy(); u.GetLongName() != "" {
			return fmt.Sprintf("%s (%s)", u.GetLongName(), id.NodeID())
		}
		return id.NodeID()
	}
	return h.inst.Name
}

// rebooting reports whether a node is down only because it is rebooting to apply settings.
func rebooting(st HostedStatus, now time.Time) bool {
	if len(st.Stops) == 0 {
		return false
	}
	last := st.Stops[len(st.Stops)-1]
	return last.Reboot && now.Sub(time.UnixMilli(last.Time)) < startGrace
}

// InstanceLog is the log of the instance with that name, or false.
func (x *Hosting) InstanceLog(name string) ([]LogLine, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, e := range x.nodes {
		if e.hn.Instance().Name == name {
			return e.hn.Log(), true
		}
	}
	return nil, false
}

// who names the node for a health problem.
func (e *hostedEntry) who() string {
	if e.role == "persona" {
		return "relay persona"
	}
	return "identity " + e.hn.label()
}
