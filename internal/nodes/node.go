package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/ScotMesh/RepeaterTastic/api/meshtastic"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/mtclient"
	"github.com/ScotMesh/RepeaterTastic/internal/wire"
)

// Node is a real Meshtastic node behind the client API standing in for one of a host's
// identities: its mesh.Remote. It keeps the identity and the host current as the node reconnects,
// and hands what the node delivers to the host.
type Node struct {
	addr     string
	stateDir string
	client   *mtclient.Client
	logf     func(string, ...any)

	mu    sync.Mutex
	host  *mesh.Host
	id    *mesh.Identity
	owner *pb.User // names the node should carry (hosted nodes)
	// seed is the saved identity a hosted node runs: its key, and the names and channels a fresh
	// node starts with. nil for a node that keeps its own identity.
	seed      *mesh.IdentityRecord
	seedTries int // key pushes that didn't take, in a row
	pushTries int // settings pushes that failed, in a row
	// lastPush and samePushes catch a node that comes back without the settings it was given;
	// stuck says which (see repeating).
	lastPush   string
	samePushes int
	stuck      string
	// extra adds settings a node needs for its part (a board's MQTT proxy), in the same edit.
	extra func(mtclient.Snapshot) []*pb.AdminMessage
	// hopsBehind is how far the node is from the air (1 behind a board): its hop limit is that much
	// higher, so its packets reach as far as the relay's.
	hopsBehind uint32
	// senTel, when set, says which telemetry the sensors the node carries need, and how often it
	// broadcasts them (docs/sensors.md). nil leaves the node's own settings alone.
	senTel *sensorTelemetry
	// afterConfigured, when set, is called with the node's state each time it has been read.
	afterConfigured func(mtclient.Snapshot)

	// rebootWait is how long a settings change waits for the node to reboot before re-reading it.
	rebootWait time.Duration
	// committed is when settings were last committed (Unix ms): meshtasticd reboots to apply
	// some, so an exit soon after is expected.
	committed atomic.Int64
	// editMu keeps settings changes one at a time: each reads the node, edits it and waits for it
	// to come back before the next one looks.
	editMu sync.Mutex
}

var _ mesh.Remote = (*Node)(nil)

// Client is the node's API client.
func (n *Node) Client() *mtclient.Client { return n.client }

// Identity creates the host's identity for the node. It waits up to wait for the node to answer,
// then falls back to the node's saved state, then to a placeholder that is replaced when the node
// first answers.
func (n *Node) Identity(ctx context.Context, wait time.Duration) (*mesh.Identity, error) {
	if seed := n.Seed(); seed != nil {
		// The record says who the node is: no need to wait for it.
		if s := n.client.Snapshot(); s.Connected && n.seeded(s) {
			st := remoteState(s)
			n.saveState(st)
			return mesh.NewHostedIdentity(n, st, *seed)
		}
		st, err := mesh.RecordState(*seed)
		if err != nil {
			return nil, err
		}
		return mesh.NewHostedIdentity(n, st, *seed)
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	err := n.client.WaitReady(wctx)
	cancel()
	var st mesh.RemoteState
	if err == nil {
		st = remoteState(n.client.Snapshot())
		n.saveState(st)
	} else {
		var ok bool
		if st, ok = n.loadState(); ok {
			n.logf("meshtasticd: node at %s not answering yet (%v); using its saved state", n.addr, err)
		} else {
			n.logf("meshtasticd: node at %s not answering yet (%v); it appears once it does", n.addr, err)
			st = placeholderState(n.addr)
		}
	}
	return mesh.NewRemoteIdentity(n, st)
}

// SetSeed makes the node run the saved identity rec: it is given rec's key (and, while it is
// fresh, rec's names and channels) and stands for rec from then on.
func (n *Node) SetSeed(rec mesh.IdentityRecord) {
	n.mu.Lock()
	n.seed = &rec
	n.mu.Unlock()
}

// Seed is the saved identity the node runs, or nil.
func (n *Node) Seed() *mesh.IdentityRecord {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.seed
}

// seedKey is the seed's key pair, or nils.
func (n *Node) seedKey() (priv, pub []byte) {
	seed := n.Seed()
	if seed == nil {
		return nil, nil
	}
	priv, err := seed.Key()
	if err != nil {
		return nil, nil
	}
	pub, err = wire.PublicKey(priv)
	if err != nil {
		return nil, nil
	}
	return priv, pub
}

// seeded reports whether the node already holds the seed's key (true without a seed).
func (n *Node) seeded(s mtclient.Snapshot) bool {
	_, pub := n.seedKey()
	return pub == nil || bytes.Equal(s.Config.GetSecurity().GetPublicKey(), pub)
}

// maxSeedTries is how often a node is given its key before RepeaterTastic gives up on it.
const maxSeedTries = 3

// OnAir reports whether the node may transmit: it holds its saved key (if it has one).
func (n *Node) OnAir() bool { return n.seeded(n.client.Snapshot()) }

// SetExtraSettings adds settings fn returns to every settings change.
func (n *Node) SetExtraSettings(fn func(mtclient.Snapshot) []*pb.AdminMessage) {
	n.mu.Lock()
	n.extra = fn
	n.mu.Unlock()
}

// sensorTelemetry is the telemetry a node carrying sensors is asked for: the environment module for
// temperature, humidity, voltage and current, and the air quality module for the particulate values.
type sensorTelemetry struct {
	env   bool
	air   bool
	every time.Duration
}

// SetSensorTelemetry makes the node broadcast the sensors it carries every so often, or, with both
// off, makes sure it broadcasts none. It is set only for a host that has the sensors feature on: a
// node that is never told keeps whatever settings it has.
func (n *Node) SetSensorTelemetry(env, air bool, every time.Duration) {
	n.mu.Lock()
	n.senTel = &sensorTelemetry{env: env, air: air, every: every}
	n.mu.Unlock()
}

// SetAfterConfigured sets a hook called with the node's state whenever it has been read, after the
// host has been brought in line with it.
func (n *Node) SetAfterConfigured(fn func(mtclient.Snapshot)) {
	n.mu.Lock()
	n.afterConfigured = fn
	n.mu.Unlock()
}

// SetHopsBehind says how many hops from the air the node is.
func (n *Node) SetHopsBehind(hops uint32) {
	n.mu.Lock()
	n.hopsBehind = hops
	n.mu.Unlock()
}

// Bind makes id, on host h, the identity the node stands for.
func (n *Node) Bind(h *mesh.Host, id *mesh.Identity) {
	n.mu.Lock()
	n.host, n.id = h, id
	n.mu.Unlock()
}

// Run keeps the bound host current with the node (settings, identity, deliveries) until ctx ends.
func (n *Node) Run(ctx context.Context) {
	n.mu.Lock()
	h := n.host
	n.mu.Unlock()
	events, stop := n.client.Subscribe(512)
	defer stop()
	if n.client.Snapshot().Connected {
		n.configured(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			switch e.Kind {
			case mtclient.Configured:
				n.configured(ctx)
			case mtclient.Disconnected:
				n.logf("meshtasticd: node at %s disconnected: %v", n.addr, e.Err)
			case mtclient.Received:
				// A sim-radio meshtasticd hands its transmissions to the client; nobody bridges them here.
				if p := e.FromRadio.GetPacket(); p != nil && p.GetDecoded().GetPortnum() != pb.PortNum_SIMULATOR_APP {
					n.mu.Lock()
					cur := n.id
					n.mu.Unlock()
					h.RemoteReceived(cur, p)
				}
			}
		}
	}
}

// configured brings the host in line with the node after a handshake.
func (n *Node) configured(ctx context.Context) {
	snap := n.client.Snapshot()
	n.mu.Lock()
	h, cur, seed := n.host, n.id, n.seed
	n.mu.Unlock()
	if !n.seeded(snap) {
		n.giveKey(ctx, h, cur)
		return
	}
	n.mu.Lock()
	n.seedTries = 0
	n.mu.Unlock()
	st := remoteState(snap)
	n.saveState(st)
	if cur.NodeNum != st.NodeNum {
		next, ok := n.follow(h, cur, seed, st)
		if !ok {
			return
		}
		cur = next
	}
	// Pushing may wait for a reboot; the event loop keeps delivering meanwhile.
	go n.pushSettings(ctx, h, cur.NodeID())
	h.SyncRemote(cur, st)
	if cur.Hosted() {
		if err := h.SaveIdentities(); err != nil {
			n.logf("meshtasticd: saving %s: %v", cur.NodeID(), err)
		}
	}
	n.logf("meshtasticd: node %s %q on %s, firmware %s, %s %s", cur.NodeID(), st.User.GetLongName(), n.addr,
		snap.Metadata.GetFirmwareVersion(), snap.Config.GetLora().GetRegion(), snap.Config.GetLora().GetModemPreset())
	n.mu.Lock()
	after := n.afterConfigured
	n.mu.Unlock()
	if after != nil {
		after(snap)
	}
}

// giveKey gives a fresh node (or one whose key was changed) the saved identity first. It comes
// back under the saved node number. Nothing it sends goes on air meanwhile.
func (n *Node) giveKey(ctx context.Context, h *mesh.Host, cur *mesh.Identity) {
	n.mu.Lock()
	n.seedTries++
	tries := n.seedTries
	n.mu.Unlock()
	if tries > maxSeedTries {
		if tries == maxSeedTries+1 {
			n.logf("meshtasticd: ERROR node at %s won't keep %s's key after %d tries; it stays off air until restarted", n.addr, cur.NodeID(), maxSeedTries)
		}
		return
	}
	n.logf("meshtasticd: node at %s gets %s's key", n.addr, cur.NodeID())
	go n.applySeed(ctx, h, cur)
}

// applySeed pushes the saved identity, reconnecting to try again if the node doesn't take it.
func (n *Node) applySeed(ctx context.Context, h *mesh.Host, cur *mesh.Identity) {
	err := n.ApplyConfig(ctx, h.Config())
	if err == nil || ctx.Err() != nil {
		return
	}
	n.logf("meshtasticd: node at %s didn't take %s's key: %v; trying again", n.addr, cur.NodeID(), err)
	select { // a node just started may not answer yet: try again on a fresh connection
	case <-ctx.Done():
	case <-time.After(n.rebootWait):
		if !n.OnAir() {
			n.client.Reconnect()
		}
	}
}

// follow moves the node's identity to the node number it now reports. It returns the new
// identity, or false (having logged why) when the identity couldn't follow.
func (n *Node) follow(h *mesh.Host, cur *mesh.Identity, seed *mesh.IdentityRecord, st mesh.RemoteState) (*mesh.Identity, bool) {
	var next *mesh.Identity
	var err error
	if seed != nil {
		next, err = mesh.NewHostedIdentity(n, st, *seed)
	} else {
		next, err = mesh.NewRemoteIdentity(n, st)
	}
	if err == nil {
		err = h.SwapRemote(cur, next)
	}
	if err != nil {
		n.logf("meshtasticd: node is now %s but the identity couldn't follow: %v", wire.NodeID(st.NodeNum), err)
		return nil, false
	}
	n.logf("meshtasticd: node %s replaces %s", next.NodeID(), cur.NodeID())
	n.mu.Lock()
	n.id = next
	n.mu.Unlock()
	return next, true
}

// pushSettings applies the host's settings to node id, reconnecting to try again on failure.
func (n *Node) pushSettings(ctx context.Context, h *mesh.Host, id string) {
	err := n.ApplyConfig(ctx, h.Config())
	n.mu.Lock()
	if err == nil {
		n.pushTries = 0
	} else {
		n.pushTries++
	}
	tries := n.pushTries
	n.mu.Unlock()
	if err == nil || ctx.Err() != nil {
		return
	}
	if tries > maxSeedTries {
		n.logf("meshtasticd: ERROR node %s didn't take the host's settings after %d tries: %v", id, maxSeedTries, err)
		return
	}
	n.logf("meshtasticd: node %s didn't take the host's settings: %v; trying again", id, err)
	select { // on a fresh connection: the node may have moved to a new number
	case <-ctx.Done():
	case <-time.After(n.rebootWait):
		n.client.Reconnect()
	}
}

// ------------------------------------------------------------------------------ mesh.Remote

func (n *Node) SendPacket(p *pb.MeshPacket) (uint32, error) { return n.client.SendPacket(p) }

func (n *Node) Admin(ctx context.Context, m *pb.AdminMessage) (*pb.AdminMessage, error) {
	return n.client.Admin(ctx, m)
}

// ------------------------------------------------------------------------------ state cache

type savedState struct {
	NodeNum  uint32            `json:"node_num"`
	User     json.RawMessage   `json:"user"`
	Channels []json.RawMessage `json:"channels"`
	Address  string            `json:"address"`
}

func (n *Node) statePath() string {
	if n.stateDir == "" {
		return ""
	}
	return filepath.Join(n.stateDir, "node.json")
}

func (n *Node) saveState(st mesh.RemoteState) {
	path := n.statePath()
	if path == "" {
		return
	}
	s := savedState{NodeNum: st.NodeNum, Address: n.addr}
	s.User, _ = protojson.Marshal(st.User)
	for _, ch := range st.Channels {
		b, _ := protojson.Marshal(ch)
		s.Channels = append(s.Channels, b)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

func (n *Node) loadState() (mesh.RemoteState, bool) {
	path := n.statePath()
	if path == "" {
		return mesh.RemoteState{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return mesh.RemoteState{}, false
	}
	var s savedState
	if json.Unmarshal(b, &s) != nil || s.NodeNum == 0 {
		return mesh.RemoteState{}, false
	}
	st := mesh.RemoteState{NodeNum: s.NodeNum, User: &pb.User{}}
	_ = protojson.Unmarshal(s.User, st.User)
	for _, raw := range s.Channels {
		ch := &pb.Channel{}
		if protojson.Unmarshal(raw, ch) == nil {
			st.Channels = append(st.Channels, ch)
		}
	}
	return st, true
}

// placeholderState names a node that hasn't answered yet, with a stable number from its address.
func placeholderState(addr string) mesh.RemoteState {
	num := crc32.ChecksumIEEE([]byte("meshtastic:"+addr)) | 0x10000000
	if num == wire.Broadcast {
		num--
	}
	return mesh.RemoteState{NodeNum: num, User: &pb.User{LongName: "Hosted node (starting)", ShortName: "…"}}
}

// ------------------------------------------------------------------------------ conversions

func remoteState(s mtclient.Snapshot) mesh.RemoteState {
	st := mesh.RemoteState{NodeNum: s.NodeNum(), Channels: s.Channels}
	if self := s.Self(); self != nil && self.User != nil {
		st.User = proto.Clone(self.User).(*pb.User)
	} else {
		st.User = &pb.User{}
	}
	if len(st.User.PublicKey) == 0 {
		st.User.PublicKey = s.Config.GetSecurity().GetPublicKey()
	}
	if st.User.Role == pb.Config_DeviceConfig_CLIENT {
		st.User.Role = s.Config.GetDevice().GetRole()
	}
	for _, n := range s.Nodes {
		st.Nodes = append(st.Nodes, n)
	}
	return st
}

// Current is the host identity the node stands for now (nil before Bind).
func (n *Node) Current() *mesh.Identity {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.id
}

// SetOwner sets the names a hosted node is given (and keeps) on every connect.
func (n *Node) SetOwner(long, short string) {
	n.mu.Lock()
	n.owner = &pb.User{LongName: long, ShortName: short}
	n.mu.Unlock()
}

// discardLogf is the logger for callers that don't want one.
func discardLogf(string, ...any) {
	// Logging is optional: the message is dropped on purpose.
}

func newNode(addr, stateDir string, c *mtclient.Client, logf func(string, ...any)) *Node {
	if logf == nil {
		logf = discardLogf
	}
	return &Node{addr: addr, stateDir: stateDir, client: c, logf: logf, rebootWait: 8 * time.Second}
}
