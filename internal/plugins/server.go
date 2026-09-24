package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/ScotMesh/RepeaterTastic/api/plugin/v1"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/wire"
)

// session is one connected plugin's event stream.
type session struct {
	token   string
	out     chan *pluginv1.HostMessage
	done    chan struct{}
	once    sync.Once
	reason  atomic.Value // string
	dropped atomic.Uint64
}

func (s *session) send(msg *pluginv1.HostMessage) {
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.out <- msg:
	default:
		s.dropped.Add(1)
	}
}

// stop asks the plugin to stop; it should close the stream and exit.
func (s *session) stop(reason string) {
	s.send(&pluginv1.HostMessage{Msg: &pluginv1.HostMessage_Stop{Stop: &pluginv1.Stop{Reason: reason}}})
}

// close ends the stream from our side.
func (s *session) close(reason string) {
	s.once.Do(func() {
		s.reason.Store(reason)
		close(s.done)
	})
}

type ctxKey struct{}

// Permission keys the Plugin API checks.
const (
	permPacketsRead    = "packets.read"
	permNodesRead      = "nodes.read"
	permMessagesRead   = "messages.read"
	permMessagesSend   = "messages.send"
	permTracerouteSend = "traceroute.send"
	permStatusRead     = "status.read"
	permSensorsPublish = "sensors.publish"
)

// serve listens on the Unix socket (and TCP, if configured) and serves the Plugin API.
func (m *Manager) serve() (*grpc.Server, error) {
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
			defer m.recoverHandler(&err)
			ctx, err = m.authenticate(ctx)
			if err != nil {
				return nil, err
			}
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) (err error) {
			defer m.recoverHandler(&err)
			ctx, err := m.authenticate(ss.Context())
			if err != nil {
				return err
			}
			return h(srv, &authedStream{ServerStream: ss, ctx: ctx})
		}),
		// Notice attached plugins whose connection died without closing (a half-open TCP link).
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	pluginv1.RegisterPluginHostServer(srv, &hostServer{m: m})

	sock := m.socketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return nil, fmt.Errorf("plugin socket: %w", err)
	}
	_ = os.Remove(sock)
	ul, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("plugin socket: %w", err)
	}
	_ = os.Chmod(sock, 0o600)
	go func() { _ = srv.Serve(ul) }()
	if addr := m.opt.Config.Listen; addr != "" {
		if err := m.serveTCP(srv, addr); err != nil {
			srv.Stop()
			return nil, err
		}
	}
	return srv, nil
}

// serveTCP listens on addr for attached plugins.
func (m *Manager) serveTCP(srv *grpc.Server, addr string) error {
	tl, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("plugins.listen %s: %w", addr, err)
	}
	m.mu.Lock()
	m.listening = tl.Addr().String()
	m.mu.Unlock()
	m.log.Info("attached plugins can connect", "addr", tl.Addr().String())
	go func() { _ = srv.Serve(tl) }()
	return nil
}

// authenticate finds the plugin the request's bearer token belongs to and adds it to ctx.
func (m *Manager) authenticate(ctx context.Context) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	var tok string
	if v := md.Get("authorization"); len(v) > 0 {
		tok = strings.TrimPrefix(v[0], "Bearer ")
	}
	if tok == "" {
		return nil, status.Error(codes.Unauthenticated, "missing plugin token")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.plugins {
		if p.hasToken(tok) {
			return context.WithValue(ctx, ctxKey{}, p), nil
		}
	}
	return nil, status.Error(codes.Unauthenticated, "unknown plugin token")
}

// hasToken reports whether tok is the plugin's token: the saved one for an attached plugin, the
// current process's for a managed one.
func (p *plugin) hasToken(tok string) bool {
	if p.rec.Attached {
		return tokenMatches(hashToken(tok), p.rec.TokenHash)
	}
	return tokenMatches(tok, p.token)
}

// recoverHandler turns a handler panic into an error: a bug in a handler must not take the daemon
// (and its radios) down with it. Defer it directly.
func (m *Manager) recoverHandler(err *error) {
	if v := recover(); v != nil {
		m.log.Error("plugin API handler panicked", "panic", v)
		*err = status.Error(codes.Internal, "internal error")
	}
}

type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authedStream) Context() context.Context { return s.ctx }

type hostServer struct {
	pluginv1.UnimplementedPluginHostServer
	m *Manager
}

func pluginFrom(ctx context.Context) *plugin { return ctx.Value(ctxKey{}).(*plugin) }

// granted checks a permission and that the plugin has a session open.
func (h *hostServer) granted(ctx context.Context, perm string) (*plugin, error) {
	p := pluginFrom(ctx)
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if p.sess == nil {
		return nil, status.Error(codes.FailedPrecondition, "open the Session stream first")
	}
	if perm != "" {
		if _, granted, _ := p.effective(); !slices.Contains(granted, perm) {
			return nil, status.Errorf(codes.PermissionDenied, "the plugin hasn't been granted %s", perm)
		}
	}
	return p, nil
}

func (h *hostServer) Session(stream pluginv1.PluginHost_SessionServer) error {
	m := h.m
	p := pluginFrom(stream.Context())
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if err := checkHello(p, hello); err != nil {
		return err
	}
	sess, granted, settings, err := m.openSession(p, hello)
	if err != nil {
		return err
	}
	defer m.endSession(p, sess)

	welcome := &pluginv1.Welcome{ApiVersion: APIVersion, HostVersion: m.opt.Version, Permissions: granted,
		SettingsJson: jsonString(settings), Radios: m.radiosProto()}
	if !p.rec.Attached {
		welcome.DataDir = m.dataRoot() + string(os.PathSeparator) + p.id
	}
	if err := stream.Send(&pluginv1.HostMessage{Msg: &pluginv1.HostMessage_Welcome{Welcome: welcome}}); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	for _, r := range m.opt.Radios {
		go m.pump(ctx, sess, p, r, granted)
	}
	if slices.Contains(granted, permStatusRead) {
		go m.pumpStatus(ctx, sess)
	}
	recvErr := make(chan error, 1)
	go func() { recvErr <- h.receive(stream, p) }()
	return forward(stream, sess, recvErr)
}

// checkHello checks the plugin's Hello names this plugin and speaks our API.
func checkHello(p *plugin, hello *pluginv1.Hello) error {
	switch {
	case hello == nil:
		return status.Error(codes.InvalidArgument, "the first message must be Hello")
	case hello.PluginId != p.id:
		return status.Errorf(codes.PermissionDenied, "this token belongs to %s, not %s", p.id, hello.PluginId)
	case hello.ApiVersion != APIVersion:
		return status.Errorf(codes.FailedPrecondition, "plugin speaks API %d; this RepeaterTastic speaks %d", hello.ApiVersion, APIVersion)
	}
	return nil
}

// openSession makes a new session the plugin's current one, replacing any old one, once the
// plugin is allowed to run. It returns the grants and settings the session starts with.
func (m *Manager) openSession(p *plugin, hello *pluginv1.Hello) (sess *session, granted []string, settings map[string]any, err error) {
	m.mu.Lock()
	if err := m.updateManifestLocked(p, hello.ManifestYaml); err != nil {
		m.mu.Unlock()
		return nil, nil, nil, err
	}
	if st, detail := p.blocker(); st != "" && (st != "waiting" || !p.rec.Attached) {
		p.state, p.detail = st, detail
		m.mu.Unlock()
		m.notify(p.id)
		return nil, nil, nil, status.Errorf(codes.FailedPrecondition, "the plugin can't run: %s", strings.TrimSpace(st+" "+detail))
	}
	tok := ""
	if !p.rec.Attached {
		tok = p.token
	}
	sess = &session{token: tok, out: make(chan *pluginv1.HostMessage, 2048), done: make(chan struct{})}
	old := p.sess
	p.sess, p.connected, p.status, p.panel = sess, time.Now(), nil, ""
	p.state, p.detail = "running", ""
	_, granted, settings = p.effective()
	m.mu.Unlock()
	if old != nil {
		old.close("replaced by a new session")
	}
	p.logs.add("info", "host", fmt.Sprintf("connected (%s %s)", p.id, hello.PluginVersion))
	m.notify(p.id)
	return sess, granted, settings, nil
}

// updateManifestLocked saves the manifest an attached plugin sent, if it changed.
func (m *Manager) updateManifestLocked(p *plugin, manifestYAML string) error {
	if !p.rec.Attached || manifestYAML == "" || manifestYAML == p.rec.ManifestYAML {
		return nil
	}
	man, err := ParseManifest([]byte(manifestYAML))
	if err == nil && man.ID != p.id {
		err = fmt.Errorf("its manifest id is %s", man.ID)
	}
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "manifest: %v", err)
	}
	p.rec.ManifestYAML, p.manifest = manifestYAML, man
	_ = m.saveLocked()
	return nil
}

// endSession closes sess and, if it's still the plugin's current one, shows the plugin as waiting
// to reconnect.
func (m *Manager) endSession(p *plugin, sess *session) {
	sess.close("stream ended")
	m.mu.Lock()
	if p.sess == sess {
		p.sess = nil
		if p.state == "running" {
			p.state, p.detail = "starting", "Disconnected; waiting for the plugin to connect again"
			if p.rec.Attached {
				p.state = "waiting"
			}
		}
	}
	m.mu.Unlock()
	p.logs.add("info", "host", "disconnected")
	m.notify(p.id)
}

// forward sends the session's messages to the plugin until the stream or the session ends.
func forward(stream pluginv1.PluginHost_SessionServer, sess *session, recvErr <-chan error) error {
	for {
		select {
		case msg := <-sess.out:
			if err := stream.Send(msg); err != nil {
				return err
			}
			if msg.GetStop() != nil {
				return awaitClose(recvErr)
			}
		case err := <-recvErr:
			return ignoreEOF(err)
		case <-sess.done:
			reason, _ := sess.reason.Load().(string)
			return status.Error(codes.Unavailable, reason)
		}
	}
}

// awaitClose gives a plugin that was asked to stop a moment to close the stream itself.
func awaitClose(recvErr <-chan error) error {
	select {
	case err := <-recvErr:
		return ignoreEOF(err)
	case <-time.After(stopGrace):
		return status.Error(codes.Unavailable, "stopped")
	}
}

func ignoreEOF(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "EOF") {
		return nil
	}
	return err
}

// receive handles what the plugin sends on the session.
func (h *hostServer) receive(stream pluginv1.PluginHost_SessionServer, p *plugin) error {
	m := h.m
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		switch v := msg.Msg.(type) {
		case *pluginv1.PluginMessage_Status:
			st := v.Status
			if len(st.Fields) > 40 {
				continue
			}
			m.mu.Lock()
			p.status = st
			m.mu.Unlock()
			m.notify(p.id)
		case *pluginv1.PluginMessage_Log:
			level := v.Log.Level
			if !slices.Contains([]string{"debug", "info", "warn", "error"}, level) {
				level = "info"
			}
			p.logs.add(level, "plugin", v.Log.Message)
		case *pluginv1.PluginMessage_Panel:
			if len(v.Panel.Json) > 1<<20 || !json.Valid([]byte(v.Panel.Json)) {
				p.logs.add("warn", "host", "ignored panel data: not JSON or larger than 1 MB")
				continue
			}
			m.mu.Lock()
			p.panel = v.Panel.Json
			m.mu.Unlock()
			m.notify(p.id)
		}
	}
}

// pump forwards one radio's bus events the plugin may see.
// tracesFor: traceroute results reach a plugin for the identity it sends them from.
func (m *Manager) tracesFor(p *plugin, r Radio, identity string) bool {
	m.mu.Lock()
	_, _, settings := p.effective()
	var schema []Setting
	if p.manifest != nil {
		schema = p.manifest.Settings
	}
	m.mu.Unlock()
	chosen := chosenIdentities(schema, settings, r.Host)
	if len(chosen) == 0 {
		relay := r.Host.Relay()
		return relay != nil && relay.NodeID() == identity
	}
	return slices.ContainsFunc(chosen, func(id *mesh.Identity) bool { return id.NodeID() == identity })
}

func (m *Manager) pump(ctx context.Context, sess *session, p *plugin, r Radio, granted []string) {
	has := func(perm string) bool { return slices.Contains(granted, perm) }
	if !has(permPacketsRead) && !has(permNodesRead) && !has(permMessagesRead) && !has(permTracerouteSend) {
		return
	}
	events, unsubscribe := r.Host.Bus.Subscribe(512)
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.done:
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if msg := m.eventMessage(p, r, e, has); msg != nil {
				sess.send(msg)
			}
		}
	}
}

// eventMessage is the message for a bus event, or nil when the plugin may not see it.
func (m *Manager) eventMessage(p *plugin, r Radio, e mesh.Event, has func(string) bool) *pluginv1.HostMessage {
	switch e.Type {
	case "packet":
		if rec, ok := e.Data.(mesh.PacketRecord); ok && has(permPacketsRead) {
			return packetEvent(r, rec)
		}
	case "node":
		if id, ok := e.Data.(string); ok && has(permNodesRead) {
			return nodeEvent(r, id)
		}
	case "message":
		if me, ok := e.Data.(mesh.MessageEvent); ok && has(permMessagesRead) {
			return textEvent(r, me)
		}
	case "traceroute":
		if tr, ok := e.Data.(mesh.TracerouteResult); ok && canSeeTraceroutes(has) {
			return m.tracerouteEvent(p, r, tr)
		}
	}
	return nil
}

// canSeeTraceroutes reports whether a plugin with these grants may see traceroute results.
func canSeeTraceroutes(has func(string) bool) bool {
	return has(permTracerouteSend) || has(permNodesRead)
}

// nodeEvent is the message for a node database change, or nil if the node isn't there.
func nodeEvent(r Radio, id string) *pluginv1.HostMessage {
	num, err := wire.ParseNodeID(id)
	if err != nil {
		return nil
	}
	n, ok := r.Host.DB.Get(num)
	if !ok {
		return nil
	}
	return &pluginv1.HostMessage{Msg: &pluginv1.HostMessage_Node{Node: &pluginv1.NodeEvent{Node: nodeProto(r.ID, n)}}}
}

// tracerouteEvent is the message for a traceroute result, or nil if it's not from an identity the
// plugin sends from.
func (m *Manager) tracerouteEvent(p *plugin, r Radio, tr mesh.TracerouteResult) *pluginv1.HostMessage {
	if !m.tracesFor(p, r, tr.Identity) {
		return nil
	}
	return &pluginv1.HostMessage{Msg: &pluginv1.HostMessage_Traceroute{Traceroute: &pluginv1.TracerouteEvent{
		RadioId: r.ID, IdentityNodeId: tr.Identity, TargetNodeId: tr.Target, Route: tr.Route,
		SnrTowards: tr.SNRTowards, RouteBack: tr.RouteBack, SnrBack: tr.SNRBack}}}
}

func (h *hostServer) ListRadios(ctx context.Context, _ *pluginv1.ListRadiosRequest) (*pluginv1.ListRadiosResponse, error) {
	if _, err := h.granted(ctx, ""); err != nil {
		return nil, err
	}
	return &pluginv1.ListRadiosResponse{Radios: h.m.radiosProto()}, nil
}

func (h *hostServer) ListNodes(ctx context.Context, req *pluginv1.ListNodesRequest) (*pluginv1.ListNodesResponse, error) {
	if _, err := h.granted(ctx, permNodesRead); err != nil {
		return nil, err
	}
	out := &pluginv1.ListNodesResponse{}
	for _, r := range h.m.opt.Radios {
		if req.RadioId != "" && r.ID != req.RadioId {
			continue
		}
		for _, n := range r.Host.DB.Snapshot() {
			out.Nodes = append(out.Nodes, nodeProto(r.ID, n))
		}
	}
	return out, nil
}

func (h *hostServer) SendText(ctx context.Context, req *pluginv1.SendTextRequest) (*pluginv1.SendResponse, error) {
	p, err := h.granted(ctx, permMessagesSend)
	if err != nil {
		return nil, err
	}
	r := h.m.radio(req.RadioId)
	if r == nil {
		return nil, status.Errorf(codes.NotFound, "no radio %q", req.RadioId)
	}
	to := wire.Broadcast
	if req.To != "" {
		if to, err = wire.ParseNodeID(req.To); err != nil {
			return nil, status.Error(codes.InvalidArgument, "to must be a node id like !a1c40e07")
		}
	}
	if n := len(req.Text); n == 0 || n > 200 {
		return nil, status.Error(codes.InvalidArgument, "text must be 1-200 bytes")
	}
	if !r.Host.Transmits() {
		return nil, notTransmitting(r)
	}
	if ok, wait := p.msgBudget.take(); !ok {
		return nil, budgetError("messages", p.msgBudget.rate(), wait)
	}
	relay := r.Host.Relay()
	pid, err := r.Host.SendText(relay, to, int(req.Channel), req.Text, req.WantAck)
	if errors.Is(err, mesh.ErrNotTransmitting) {
		p.msgBudget.refund()
		return nil, notTransmitting(r)
	}
	if err != nil {
		if pid == 0 {
			p.msgBudget.refund() // refused before anything was queued
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	p.logs.add("info", "host", fmt.Sprintf("sent a message from %s to %s on %s", relay.NodeID(), nodeOrChannel(req.To, req.Channel), r.ID))
	return &pluginv1.SendResponse{PacketId: pid}, nil
}

func (h *hostServer) Traceroute(ctx context.Context, req *pluginv1.TracerouteRequest) (*pluginv1.SendResponse, error) {
	p, err := h.granted(ctx, permTracerouteSend)
	if err != nil {
		return nil, err
	}
	r := h.m.radio(req.RadioId)
	if r == nil {
		return nil, status.Errorf(codes.NotFound, "no radio %q", req.RadioId)
	}
	target, err := wire.ParseNodeID(req.Target)
	if err != nil || target == wire.Broadcast {
		return nil, status.Error(codes.InvalidArgument, "target must be a node id like !a1c40e07")
	}
	// A plugin sends only from the identity chosen for it in its settings on that radio, or the
	// relay persona when none is chosen there.
	h.m.mu.Lock()
	_, _, settings := p.effective()
	var schema []Setting
	if p.manifest != nil {
		schema = p.manifest.Settings
	}
	h.m.mu.Unlock()
	allowed := chosenIdentities(schema, settings, r.Host)
	from := r.Host.Relay()
	if len(allowed) > 0 {
		from = allowed[0]
	}
	if req.From != "" {
		num, err := wire.ParseNodeID(req.From)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "from must be a node id like !a1c40e07")
		}
		if !slices.ContainsFunc(append(allowed, from), func(id *mesh.Identity) bool { return id.NodeNum == num }) {
			return nil, status.Errorf(codes.PermissionDenied, "%s isn't the identity chosen in the plugin's settings for %s", req.From, r.ID)
		}
		from = r.Host.Identity(num)
	}
	if !r.Host.Transmits() {
		return nil, notTransmitting(r)
	}
	if ok, wait := p.trBudget.take(); !ok {
		return nil, budgetError("traceroutes", p.trBudget.rate(), wait)
	}
	if err := r.Host.Traceroute(from, target); err != nil {
		p.trBudget.refund() // nothing was sent (e.g. the identity's 30 s traceroute limit)
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	}
	p.logs.add("info", "host", fmt.Sprintf("sent a traceroute from %s to %s on %s", from.NodeID(), req.Target, r.ID))
	return &pluginv1.SendResponse{}, nil
}

// chosenIdentities are the identities on this radio picked in the plugin's "identities" settings.
func chosenIdentities(schema []Setting, settings map[string]any, host *mesh.Host) []*mesh.Identity {
	var out []*mesh.Identity
	for _, s := range schema {
		if s.Type != "identities" {
			continue
		}
		for _, str := range settingNames(settings[s.Key]) {
			num, err := wire.ParseNodeID(str)
			if err != nil {
				continue
			}
			if id := host.Identity(num); id != nil && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// settingNames is a list setting's value as names, whether it was saved as []string or read back
// from JSON as []any.
func settingNames(v any) []string {
	switch v := v.(type) {
	case []string:
		return v
	case []any:
		var names []string
		for _, x := range v {
			if str, ok := x.(string); ok {
				names = append(names, str)
			}
		}
		return names
	}
	return nil
}

// notTransmitting is the error for a send on a radio that's in monitor mode or off.
func notTransmitting(r *Radio) error {
	return status.Errorf(codes.FailedPrecondition, "%s isn't transmitting (monitor or off)", r.ID)
}

func budgetError(what string, perHour int, wait time.Duration) error {
	if perHour <= 0 {
		return status.Errorf(codes.PermissionDenied, "plugins may not send %s (plugins.%s_per_hour is 0)", what, what)
	}
	return status.Errorf(codes.ResourceExhausted, "send budget used up (%d %s an hour); try again in %s", perHour, what, wait.Round(time.Second))
}

func nodeOrChannel(to string, ch uint32) string {
	if to != "" {
		return to
	}
	return fmt.Sprintf("channel %d", ch)
}
