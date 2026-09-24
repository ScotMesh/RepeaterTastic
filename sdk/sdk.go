// Package sdk connects a Go program to RepeaterTastic as a plugin.
//
//	c, err := sdk.Connect(ctx, sdk.Options{ID: "hello", Version: "1.0.0"})
//	if err != nil { log.Fatal(err) }
//	defer c.Close()
//	for msg := range c.Events() {
//		if p := msg.GetPacket(); p != nil { ... }
//	}
//
// A managed plugin (started by RepeaterTastic) finds the socket and token in its environment.
// An attached plugin sets Options.Addr and Options.Token, and usually Options.ManifestYAML.
// See docs/plugins.md.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginv1 "github.com/ScotMesh/RepeaterTastic/api/plugin/v1"
)

// Options say how to reach RepeaterTastic. Empty fields come from the environment
// (RT_PLUGIN_ID, RT_PLUGIN_SOCKET, RT_PLUGIN_ADDR, RT_PLUGIN_TOKEN).
type Options struct {
	ID      string
	Version string
	Socket  string // Unix socket (managed plugins)
	Addr    string // host:port (attached plugins)
	Token   string
	// ManifestYAML is sent by attached plugins so the GUI can show their details and settings.
	ManifestYAML string
}

// Client is a connected plugin session.
type Client struct {
	Host    pluginv1.PluginHostClient
	Welcome *pluginv1.Welcome

	conn    *grpc.ClientConn
	stream  pluginv1.PluginHost_SessionClient
	events  chan *pluginv1.HostMessage
	ctx     context.Context
	cancel  context.CancelFunc
	sendMu  sync.Mutex
	setMu   sync.Mutex
	setting string
	err     error
}

// heartbeatInterval is how often the plugin tells RepeaterTastic it is alive (a variable for tests).
var heartbeatInterval = 30 * time.Second

type tokenCreds string

func (t tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}
func (tokenCreds) RequireTransportSecurity() bool { return false }

// Connect opens the session and waits for Welcome. ctx is the session's lifetime, not just the
// connection attempt: cancelling it ends the session, so don't pass a context with a timeout.
func Connect(ctx context.Context, o Options) (*Client, error) {
	env := func(v *string, name string) {
		if *v == "" {
			*v = os.Getenv(name)
		}
	}
	env(&o.ID, "RT_PLUGIN_ID")
	env(&o.Socket, "RT_PLUGIN_SOCKET")
	env(&o.Addr, "RT_PLUGIN_ADDR")
	env(&o.Token, "RT_PLUGIN_TOKEN")
	target := o.Addr
	if o.Socket != "" {
		target = "unix://" + o.Socket
	}
	switch {
	case o.ID == "":
		return nil, errors.New("sdk: no plugin id (RT_PLUGIN_ID)")
	case target == "":
		return nil, errors.New("sdk: no RepeaterTastic to connect to (RT_PLUGIN_SOCKET or RT_PLUGIN_ADDR)")
	case o.Token == "":
		return nil, errors.New("sdk: no token (RT_PLUGIN_TOKEN)")
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(tokenCreds(o.Token)))
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	c := &Client{Host: pluginv1.NewPluginHostClient(conn), conn: conn, events: make(chan *pluginv1.HostMessage, 256), ctx: sctx, cancel: cancel}
	if c.stream, err = c.Host.Session(sctx); err != nil {
		c.Close()
		return nil, err
	}
	hello := &pluginv1.Hello{PluginId: o.ID, ApiVersion: 1, PluginVersion: o.Version, ManifestYaml: o.ManifestYAML}
	if err := c.stream.Send(&pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Hello{Hello: hello}}); err != nil {
		c.Close()
		return nil, err
	}
	first, err := c.stream.Recv()
	if err != nil {
		c.Close()
		return nil, err
	}
	if c.Welcome = first.GetWelcome(); c.Welcome == nil {
		c.Close()
		return nil, errors.New("sdk: RepeaterTastic didn't send Welcome")
	}
	c.setting = c.Welcome.SettingsJson
	go c.readLoop()
	go c.heartbeat()
	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.events)
	defer c.cancel()
	for {
		msg, err := c.stream.Recv()
		if err != nil {
			c.err = err
			return
		}
		if s := msg.GetSettings(); s != nil {
			c.setMu.Lock()
			c.setting = s.SettingsJson
			c.setMu.Unlock()
		}
		select {
		case c.events <- msg:
		case <-c.ctx.Done():
			return
		}
		if msg.GetStop() != nil {
			return
		}
	}
}

func (c *Client) heartbeat() {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			_ = c.send(&pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Heartbeat{Heartbeat: &pluginv1.Heartbeat{}}})
		}
	}
}

// Events delivers what RepeaterTastic sends: packets, nodes, messages, traceroutes, settings
// changes, panel actions and Stop. It closes when the session ends.
func (c *Client) Events() <-chan *pluginv1.HostMessage { return c.events }

// Context ends when the session does (Stop received, RepeaterTastic gone, or Close).
func (c *Client) Context() context.Context { return c.ctx }

// Err is why the session ended, once Events has closed.
func (c *Client) Err() error { return c.err }

// Settings decodes the current settings into v.
func (c *Client) Settings(v any) error {
	c.setMu.Lock()
	s := c.setting
	c.setMu.Unlock()
	return json.Unmarshal([]byte(s), v)
}

// Status sets the one-line summary on the plugin's card; state is "ok", "warning" or "error".
func (c *Client) Status(summary, state string, fields map[string]string) error {
	return c.send(&pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Status{Status: &pluginv1.Status{Summary: summary, State: state, Fields: fields}}})
}

// Log writes a line to the plugin's log in the GUI; level is debug, info, warn or error.
func (c *Client) Log(level, format string, args ...any) error {
	return c.send(&pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Log{Log: &pluginv1.LogLine{Level: level, Message: fmt.Sprintf(format, args...)}}})
}

// PublishSensor gives a reading to one of the host's push sensors, which the identities it is
// attached to then broadcast as their own (needs the sensors.publish permission). Field names are
// the Meshtastic ones: temperature, humidity, lux, voltage, current, pm10, pm25, pm100, distance,
// radiation, rainfall_1h, rainfall_24h.
func (c *Client) PublishSensor(ctx context.Context, id string, fields map[string]float64) error {
	_, err := c.Host.PublishSensor(ctx, &pluginv1.PublishSensorRequest{SensorId: id, Fields: fields})
	return err
}

// Panel sends data for the plugin's GUI panel (JSON-encoded).
func (c *Client) Panel(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.send(&pluginv1.PluginMessage{Msg: &pluginv1.PluginMessage_Panel{Panel: &pluginv1.PanelData{Json: string(b)}}})
}

func (c *Client) send(m *pluginv1.PluginMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(m)
}

// Close ends the session.
func (c *Client) Close() {
	c.sendMu.Lock()
	if c.stream != nil {
		_ = c.stream.CloseSend()
	}
	c.sendMu.Unlock()
	c.cancel()
	_ = c.conn.Close()
}
