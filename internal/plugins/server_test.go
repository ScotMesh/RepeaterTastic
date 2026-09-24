package plugins

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/ScotMesh/RepeaterTastic/api/meshtastic"
	pluginv1 "github.com/ScotMesh/RepeaterTastic/api/plugin/v1"
	"github.com/ScotMesh/RepeaterTastic/internal/config"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/wire"
)

// apiEnv is a started Manager with one simulated radio, listening on 127.0.0.1.
type apiEnv struct {
	m      *Manager
	host   *mesh.Host
	node   *fakeNode // the relay persona's meshtasticd
	conn   *grpc.ClientConn
	client pluginv1.PluginHostClient
	ctx    context.Context
}

func newAPIEnv(t *testing.T, with ...func(*Options)) *apiEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	host, _, node := relayHost(t, ctx, quietLog())
	cfg := config.Default().Plugins
	cfg.Listen = "127.0.0.1:0"
	o := Options{Config: cfg, Dir: shortPluginDir(t), Radios: []Radio{{ID: "main", Name: "Main", Host: host}}, Version: "test", Log: quietLog()}
	for _, f := range with {
		f(&o)
	}
	m, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(m.Listening(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		cancel()
		m.Wait()
	})
	return &apiEnv{m: m, host: host, node: node, conn: conn, client: pluginv1.NewPluginHostClient(conn), ctx: ctx}
}

// addRemote adds a non-relay identity backed by its own fake node.
func addRemote(t *testing.T, h *mesh.Host, name string) *mesh.Identity {
	t.Helper()
	key, err := mesh.NewIdentity(nil, name, "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := mesh.NewRemoteIdentity(&fakeNode{}, mesh.RemoteState{NodeNum: key.NodeNum,
		User:     &pb.User{LongName: name, PublicKey: key.PublicKey},
		Channels: []*pb.Channel{{Index: 0, Role: pb.Channel_PRIMARY, Settings: &pb.ChannelSettings{Psk: []byte{1}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.AddIdentity(id); err != nil {
		t.Fatal(err)
	}
	return id
}

func withToken(ctx context.Context, tok string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
}

// attach registers an attached plugin and returns a context carrying its token.
func (e *apiEnv) attach(t *testing.T, id string, perms ...string) context.Context {
	t.Helper()
	tok, err := e.m.Attach(id, "", perms)
	if err != nil {
		t.Fatal(err)
	}
	return withToken(e.ctx, tok)
}

func hello(id, manifest string) *pluginv1.PluginMessage {
	return &pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Hello{Hello: &pluginv1.Hello{PluginId: id, ApiVersion: APIVersion, ManifestYaml: manifest}}}
}

// open starts a session and returns it after Welcome.
func (e *apiEnv) open(t *testing.T, ctx context.Context, h *pluginv1.PluginMessage) pluginv1.PluginHost_SessionClient {
	t.Helper()
	s, err := e.client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(h); err != nil {
		t.Fatal(err)
	}
	first, err := s.Recv()
	if err != nil {
		t.Fatalf("session refused: %v", err)
	}
	if first.GetWelcome() == nil {
		t.Fatalf("first message %v", first)
	}
	return s
}

// refused opens a session and returns the error that ends it.
func (e *apiEnv) refused(t *testing.T, ctx context.Context, h *pluginv1.PluginMessage) error {
	t.Helper()
	s, err := e.client.Session(ctx)
	if err != nil {
		return err
	}
	if h != nil {
		_ = s.Send(h)
	} else {
		_ = s.CloseSend()
	}
	_, err = s.Recv()
	return err
}

func wantCode(t *testing.T, what string, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Errorf("%s: %v, want %s", what, err, code)
	}
}

func TestAPIAuthentication(t *testing.T) {
	e := newAPIEnv(t)
	_, err := e.client.ListRadios(e.ctx, &pluginv1.ListRadiosRequest{})
	wantCode(t, "no token", err, codes.Unauthenticated)
	_, err = e.client.ListRadios(withToken(e.ctx, "rtp_wrong"), &pluginv1.ListRadiosRequest{})
	wantCode(t, "wrong token", err, codes.Unauthenticated)
	wantCode(t, "stream without token", e.refused(t, e.ctx, hello("x", "")), codes.Unauthenticated)
	ctx := e.attach(t, "remote", "nodes.read")
	_, err = e.client.ListRadios(ctx, &pluginv1.ListRadiosRequest{})
	wantCode(t, "no session yet", err, codes.FailedPrecondition)
}

func TestSessionRejectsBadHello(t *testing.T) {
	e := newAPIEnv(t)
	ctx := e.attach(t, "remote")
	status := &pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Status{Status: &pluginv1.Status{}}}
	oldAPI := hello("remote", "")
	oldAPI.GetHello().ApiVersion = 0
	cases := []struct {
		name string
		msg  *pluginv1.PluginMessage
		code codes.Code
	}{
		{"not hello", status, codes.InvalidArgument},
		{"other id", hello("someone", ""), codes.PermissionDenied},
		{"api", oldAPI, codes.FailedPrecondition},
		{"bad manifest", hello("remote", "id: ["), codes.InvalidArgument},
		{"foreign manifest", hello("remote", "id: other\napi: 1\n"), codes.InvalidArgument},
	}
	for _, c := range cases {
		wantCode(t, c.name, e.refused(t, ctx, c.msg), c.code)
	}
	if err := e.refused(t, ctx, nil); err == nil {
		t.Error("a stream closed before Hello wasn't an error")
	}
	if err := e.m.Disable("remote"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "disabled", e.refused(t, ctx, hello("remote", "")), codes.FailedPrecondition)
	if in, _ := e.m.Get("remote"); in.State != "disabled" {
		t.Fatalf("state %s", in.State)
	}
}

const remoteManifest = `id: remote
name: Remote
version: 2.0.0
api: 1
permissions: [nodes.read, packets.read, messages.read, messages.send, traceroute.send]
settings:
  - key: report_as
    type: identities
`

var allPerms = []string{"nodes.read", "packets.read", "messages.read", "messages.send", "traceroute.send"}

func TestSessionManifestAndReceive(t *testing.T) {
	e := newAPIEnv(t)
	ctx := e.attach(t, "remote", allPerms...)
	s := e.open(t, ctx, hello("remote", remoteManifest))
	in, _ := e.m.Get("remote")
	if in.Version != "2.0.0" || !in.Connected || in.State != "running" {
		t.Fatalf("after hello: %+v", in)
	}
	// The same manifest again is not re-parsed; a second session replaces the first.
	s2 := e.open(t, ctx, hello("remote", remoteManifest))
	if _, err := s.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("replaced session ended with %v", err)
	}

	fields := map[string]string{}
	for i := 0; i < 41; i++ {
		fields[strings.Repeat("f", i+1)] = "v"
	}
	send := func(m *pluginv1.PluginMessage) {
		t.Helper()
		if err := s2.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	send(&pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Status{Status: &pluginv1.Status{Summary: "too big", Fields: fields}}})
	send(&pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Log{Log: &pluginv1.LogLine{Level: "shout", Message: "odd level"}}})
	send(&pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Panel{Panel: &pluginv1.PanelData{Json: "{nope"}}})
	send(&pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Heartbeat{Heartbeat: &pluginv1.Heartbeat{}}})
	send(&pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Status{Status: &pluginv1.Status{Summary: "fine", State: "warning"}}})
	waitFor(t, 5*time.Second, func() bool { in, _ := e.m.Get("remote"); return in.Status != nil }, nil)
	in, _ = e.m.Get("remote")
	lines, _ := e.m.Logs("remote")
	text := logText(lines)
	if in.Status.Summary != "fine" || !strings.Contains(text, "plugin info: odd level") || !strings.Contains(text, "ignored panel data") {
		t.Fatalf("status %+v\n%s", in.Status, text)
	}

	// Closing the stream shows the plugin as waiting again.
	if err := s2.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("clean close: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { in, _ := e.m.Get("remote"); return !in.Connected && in.State == "waiting" }, nil)
}

func TestDisableStopsAttachedSession(t *testing.T) {
	e := newAPIEnv(t)
	ctx := e.attach(t, "remote")
	s := e.open(t, ctx, hello("remote", ""))
	done := make(chan error, 1)
	go func() { done <- e.m.Disable("remote") }()
	msg, err := s.Recv()
	if err != nil || msg.GetStop() == nil {
		t.Fatalf("expected Stop, got %v %v", msg, err)
	}
	_ = s.CloseSend()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if in, _ := e.m.Get("remote"); in.Connected || in.State != "disabled" {
		t.Fatalf("after disable: %+v", in)
	}
}

func TestStopWithoutClosing(t *testing.T) {
	e := newAPIEnv(t)
	ctx := e.attach(t, "remote")
	s := e.open(t, ctx, hello("remote", ""))
	start := time.Now()
	if err := e.m.Disable("remote"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < stopGrace/2 {
		t.Fatal("didn't wait for the plugin to leave")
	}
	if _, err := s.Recv(); err != nil { // the Stop
		t.Fatal(err)
	}
	if _, err := s.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("stream end: %v", err)
	}
}

func TestListCalls(t *testing.T) {
	e := newAPIEnv(t)
	ctx := e.attach(t, "remote", "messages.read")
	e.open(t, ctx, hello("remote", ""))
	e.host.DB.Update(0x0badcafe, func(n *mesh.NodeEntry) {
		n.User = &pb.User{LongName: "Far"}
		n.Position = &pb.Position{}
		n.Metrics = &pb.DeviceMetrics{}
		n.LastHeard = time.Now()
	})
	radios, err := e.client.ListRadios(ctx, &pluginv1.ListRadiosRequest{})
	if err != nil || len(radios.Radios) != 1 || radios.Radios[0].Relay == nil || radios.Radios[0].Name != "Main" {
		t.Fatalf("radios %v %v", radios, err)
	}
	_, err = e.client.ListNodes(ctx, &pluginv1.ListNodesRequest{})
	wantCode(t, "nodes without nodes.read", err, codes.PermissionDenied)

	ctx2 := e.attach(t, "reader", "nodes.read")
	e.open(t, ctx2, hello("reader", ""))
	nodes, err := e.client.ListNodes(ctx2, &pluginv1.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var far *pluginv1.Node
	for _, n := range nodes.Nodes {
		if n.NodeNum == 0x0badcafe {
			far = n
		}
	}
	var u pb.User
	if far == nil || far.RadioId != "main" || far.LastHeardMs == 0 || proto.Unmarshal(far.User, &u) != nil || u.LongName != "Far" {
		t.Fatalf("node %v", far)
	}
	if other, _ := e.client.ListNodes(ctx2, &pluginv1.ListNodesRequest{RadioId: "other"}); len(other.Nodes) != 0 {
		t.Fatalf("filtered nodes %v", other.Nodes)
	}
}

func TestSendText(t *testing.T) {
	e := newAPIEnv(t)
	ctx := e.attach(t, "remote", "messages.send")
	e.open(t, ctx, hello("remote", ""))
	req := func(radio, to, text string) error {
		_, err := e.client.SendText(ctx, &pluginv1.SendTextRequest{RadioId: radio, To: to, Text: text, Channel: 0})
		return err
	}
	wantCode(t, "unknown radio", req("nope", "", "hi"), codes.NotFound)
	wantCode(t, "bad to", req("", "bob", "hi"), codes.InvalidArgument)
	wantCode(t, "empty", req("", "", ""), codes.InvalidArgument)
	wantCode(t, "long", req("", "", strings.Repeat("x", 201)), codes.InvalidArgument)
	resp, err := e.client.SendText(ctx, &pluginv1.SendTextRequest{RadioId: "main", To: "!0badcafe", Text: "hi", WantAck: true})
	if err != nil || resp.PacketId == 0 {
		t.Fatalf("send: %v %v", resp, err)
	}
	if lines, _ := e.m.Logs("remote"); !strings.Contains(logText(lines), "to !0badcafe on main") || !e.node.sentText("hi") {
		t.Fatalf("not sent; log %s", logText(lines))
	}
	_, err = e.client.SendText(ctx, &pluginv1.SendTextRequest{Text: "hi", Channel: 99})
	wantCode(t, "no such channel", err, codes.InvalidArgument)

	if err := e.m.SetLimits(Limits{MessagesPerHour: 0, TraceroutesPerHour: 12}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "zero budget", req("", "", "hi"), codes.PermissionDenied)
	if err := e.m.SetLimits(Limits{MessagesPerHour: 6, TraceroutesPerHour: 12}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "burst of one", req("", "", "hi"), codes.OK)
	wantCode(t, "used up", req("", "", "hi"), codes.ResourceExhausted)
	if err := e.host.SetRelayRole(mesh.RoleMonitor); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "monitor", req("", "", "hi"), codes.FailedPrecondition)
}

func TestTraceroute(t *testing.T) {
	e := newAPIEnv(t)
	other := addRemote(t, e.host, "Other")
	ctx := e.attach(t, "remote", allPerms...)
	e.open(t, ctx, hello("remote", remoteManifest))
	tr := func(from, target string) error {
		_, err := e.client.Traceroute(ctx, &pluginv1.TracerouteRequest{RadioId: "main", From: from, Target: target})
		return err
	}
	_, err := e.client.Traceroute(ctx, &pluginv1.TracerouteRequest{RadioId: "x", Target: "!0badcafe"})
	wantCode(t, "unknown radio", err, codes.NotFound)
	wantCode(t, "bad target", tr("", "bob"), codes.InvalidArgument)
	wantCode(t, "broadcast", tr("", wire.NodeID(wire.Broadcast)), codes.InvalidArgument)
	wantCode(t, "bad from", tr("me", "!0badcafe"), codes.InvalidArgument)
	wantCode(t, "other not chosen", tr(other.NodeID(), "!0badcafe"), codes.PermissionDenied)
	wantCode(t, "from relay", tr(e.host.Relay().NodeID(), "!0badcafe"), codes.OK)
	wantCode(t, "identity rate limit", tr("", "!0badcafe"), codes.ResourceExhausted)

	if err := e.m.SetSettings("remote", map[string]any{"report_as": []any{other.NodeID()}}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "chosen identity", tr("", "!0badcafe"), codes.OK)
	if lines, _ := e.m.Logs("remote"); !strings.Contains(logText(lines), "traceroute from "+other.NodeID()) {
		t.Fatalf("log %s", logText(lines))
	}
	if err := e.m.SetLimits(Limits{MessagesPerHour: 30}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "zero budget", tr("", "!0badcafe"), codes.PermissionDenied)
	if err := e.host.SetRelayRole(mesh.RoleOff); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "off", tr("", "!0badcafe"), codes.FailedPrecondition)
}

// busEvents are the events published while waiting for a plugin to see them; the filtered ones
// must never reach it.
func busEvents(h *mesh.Host, other *mesh.Identity) []mesh.Event {
	relay := h.Relay().NodeID()
	pkt := &pb.MeshPacket{From: 0x0badcafe, To: 0xffffffff, Id: 9, PayloadVariant: &pb.MeshPacket_Encrypted{Encrypted: []byte{1}}}
	return []mesh.Event{
		{Type: "packet", Data: mesh.PacketRecord{Direction: "rx"}}, // no packet copy: skipped
		{Type: "packet", Data: mesh.PacketRecord{Direction: "rx", Kind: "text", Time: 5000, Mesh: pkt,
			Data:    &pb.Data{Portnum: pb.PortNum_TEXT_MESSAGE_APP, Payload: []byte("x")},
			Holders: []mesh.ChannelHolder{{NodeNum: 1, Index: 2}, {NodeNum: 2, Index: 1, Relay: true}, {NodeNum: 3, Index: 3, Relay: true}}}},
		{Type: "node", Data: "not-a-node"},
		{Type: "node", Data: "!00000042"}, // not in the DB
		{Type: "node", Data: "!0badcafe"},
		{Type: "message", Data: mesh.MessageEvent{Identity: other.NodeID(), Message: mesh.Message{Text: "not relay"}}},
		{Type: "message", Data: mesh.MessageEvent{Identity: relay, Message: mesh.Message{Direction: "out", Status: "acked", Text: "status update"}}},
		{Type: "message", Data: mesh.MessageEvent{Identity: relay, Message: mesh.Message{Direction: "in", From: "!0badcafe", To: "!ffffffff", Channel: -1, Text: "hello"}}},
		{Type: "traceroute", Data: mesh.TracerouteResult{Identity: other.NodeID(), Target: "!1"}},
		{Type: "traceroute", Data: mesh.TracerouteResult{Identity: relay, Target: "!0badcafe", Route: []string{"!2"}}},
		{Type: "identity", Data: nil},
	}
}

func TestEventsReachPlugin(t *testing.T) {
	e := newAPIEnv(t)
	other := addRemote(t, e.host, "Other")
	e.host.DB.Update(0x0badcafe, func(n *mesh.NodeEntry) { n.User = &pb.User{LongName: "Far"} })
	ctx := e.attach(t, "remote", "nodes.read", "packets.read", "messages.read")
	s := e.open(t, ctx, hello("remote", ""))

	got := make(chan *pluginv1.HostMessage, 64)
	go func() {
		for {
			m, err := s.Recv()
			if err != nil {
				close(got)
				return
			}
			got <- m
		}
	}()
	stop := make(chan struct{})
	defer close(stop)
	go publishUntil(stop, e.host, busEvents(e.host, other))
	seen := collectKinds(t, got, 4)
	checkPacket(t, seen["packet"].GetPacket(), e.host.Relay().NodeNum)
	if n := seen["node"].GetNode().GetNode(); n.GetNodeNum() != 0x0badcafe || n.GetLastHeardMs() != 0 {
		t.Errorf("node %v", n)
	}
	if tx := seen["text"].GetText(); tx.GetText() != "hello" || tx.GetDirect() || tx.GetChannel() != 0 {
		t.Errorf("text %v", tx)
	}
	if tr := seen["traceroute"].GetTraceroute(); tr.GetTargetNodeId() != "!0badcafe" || tr.GetRadioId() != "main" {
		t.Errorf("traceroute %v", tr)
	}
}

func publishUntil(stop <-chan struct{}, h *mesh.Host, events []mesh.Event) {
	for {
		for _, ev := range events {
			h.Bus.Publish(ev)
		}
		select {
		case <-stop:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// collectKinds reads messages until n kinds have been seen, failing on any filtered event.
func collectKinds(t *testing.T, got <-chan *pluginv1.HostMessage, n int) map[string]*pluginv1.HostMessage {
	t.Helper()
	seen := map[string]*pluginv1.HostMessage{}
	deadline := time.After(5 * time.Second)
	for len(seen) < n {
		select {
		case m, ok := <-got:
			if !ok {
				t.Fatal("session ended")
			}
			k := hostKind(m)
			if bad := filtered(m); bad != "" {
				t.Fatalf("filtered event delivered: %s", bad)
			}
			seen[k] = m
		case <-deadline:
			t.Fatalf("only saw %v", seen)
		}
	}
	return seen
}

func hostKind(m *pluginv1.HostMessage) string {
	switch {
	case m.GetPacket() != nil:
		return "packet"
	case m.GetNode() != nil:
		return "node"
	case m.GetText() != nil:
		return "text"
	case m.GetTraceroute() != nil:
		return "traceroute"
	}
	return "other"
}

func filtered(m *pluginv1.HostMessage) string {
	switch {
	case m.GetText() != nil && m.GetText().GetText() != "hello":
		return m.GetText().GetText()
	case m.GetTraceroute() != nil && m.GetTraceroute().GetTargetNodeId() != "!0badcafe":
		return "traceroute from another identity"
	case m.GetNode() != nil && m.GetNode().GetNode().GetNodeNum() != 0x0badcafe:
		return "unknown node"
	}
	return ""
}

func checkPacket(t *testing.T, p *pluginv1.PacketEvent, relay uint32) {
	t.Helper()
	var mp pb.MeshPacket
	if err := proto.Unmarshal(p.GetMeshPacket(), &mp); err != nil {
		t.Fatal(err)
	}
	if !p.Decoded || p.RelayChannelIndex != 1 || len(p.Holders) != 3 || p.ReporterNodeNum != relay || p.TimeMs != 5000 {
		t.Errorf("packet event %v", p)
	}
	if mp.Channel != 1 || mp.GetRxTime() != 5 || string(mp.GetDecoded().GetPayload()) != "x" {
		t.Errorf("mesh packet %v", &mp)
	}
}

func TestNoEventsWithoutReadPermissions(t *testing.T) {
	e := newAPIEnv(t)
	ctx := e.attach(t, "remote", "messages.send")
	s := e.open(t, ctx, hello("remote", ""))
	stop := make(chan struct{})
	go publishUntil(stop, e.host, busEvents(e.host, e.host.Relay()))
	time.Sleep(200 * time.Millisecond)
	close(stop)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = e.m.NewToken("remote")
	}()
	msg, err := s.Recv()
	if err == nil {
		t.Fatalf("got %v", msg)
	}
	wantCode(t, "token replaced", err, codes.Unavailable)
}

func TestEventMessageWithoutPermission(t *testing.T) {
	m := &Manager{}
	none := func(string) bool { return false }
	for _, ev := range []mesh.Event{
		{Type: "packet", Data: mesh.PacketRecord{}},
		{Type: "node", Data: "!00000001"},
		{Type: "message", Data: mesh.MessageEvent{}},
		{Type: "traceroute", Data: mesh.TracerouteResult{}},
		{Type: "packet", Data: "wrong type"},
	} {
		if msg := m.eventMessage(nil, Radio{}, ev, none); msg != nil {
			t.Errorf("%s: %v", ev.Type, msg)
		}
	}
	if !canSeeTraceroutes(func(p string) bool { return p == permNodesRead }) || canSeeTraceroutes(none) {
		t.Fatal("canSeeTraceroutes")
	}
}

func TestHelpers(t *testing.T) {
	m := &Manager{log: quietLog()}
	err := func() (err error) {
		defer m.recoverHandler(&err)
		panic("boom")
	}()
	wantCode(t, "panic", err, codes.Internal)
	for in, want := range map[error]bool{
		nil:                     true,
		context.Canceled:        true,
		io.EOF:                  true,
		errors.New("real"):      false,
		errors.New("x: EOF, y"): true,
	} {
		if (ignoreEOF(in) == nil) != want {
			t.Errorf("ignoreEOF(%v)", in)
		}
	}
	wantCode(t, "budget zero", budgetError("messages", 0, 0), codes.PermissionDenied)
	if err := budgetError("messages", 6, 90*time.Second); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "1m30s") {
		t.Errorf("budget: %v", err)
	}
	if nodeOrChannel("!1", 2) != "!1" || nodeOrChannel("", 2) != "channel 2" {
		t.Error("nodeOrChannel")
	}
	if jsonString(map[string]any{"f": func() {}}) != "{}" {
		t.Error("jsonString fallback")
	}
	s := &session{out: make(chan *pluginv1.HostMessage), done: make(chan struct{})}
	s.send(&pluginv1.HostMessage{})
	if s.dropped.Load() != 1 {
		t.Error("full session didn't count a drop")
	}
	s.close("x")
	s.close("y")
	s.send(&pluginv1.HostMessage{})
	if s.dropped.Load() != 1 || s.reason.Load() != "x" {
		t.Error("closed session")
	}
}

func TestChoicesListSiteIdentities(t *testing.T) {
	e := newAPIEnv(t)
	c := e.m.choices()
	if len(c.radios) != 1 || c.radios[0] != "main" || !slices.Contains(c.identities, e.host.Relay().NodeID()) {
		t.Fatalf("choices %+v", c)
	}
}
