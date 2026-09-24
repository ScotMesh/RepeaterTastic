# Plugin API v1 reference

[← README](../README.md) · [Plugins](plugins.md) · [HTTP API](api.md) · [Configuration](configuration.md) · [Architecture](architecture.md)

This is the reference for plugin authors. For installing, enabling and configuring plugins, see
[Plugins](plugins.md).

- **Definition:** [`api/plugin/v1/plugin.proto`](../api/plugin/v1/plugin.proto), package
  `repeatertastic.plugin.v1`, service `PluginHost`.
- **Generated Go:** `github.com/ScotMesh/RepeaterTastic/api/plugin/v1`.
- **Go SDK:** `github.com/ScotMesh/RepeaterTastic/sdk`.
- **Meshtastic protobufs:** `github.com/ScotMesh/RepeaterTastic/pb`.
- **Other languages:** generate a gRPC client from the proto.

## Contents

- [Transport and auth](#transport-and-auth)
- [Managed and attached plugins](#managed-and-attached-plugins)
- [Session](#session)
- [Calls](#calls)
- [Events](#events)
- [Model messages](#model-messages)
- [Errors](#errors)
- [Manifest (`plugin.yaml`)](#manifest-pluginyaml)
- [Panels](#panels)
- [Go SDK](#go-sdk)

## Transport and auth

| | Managed | Attached |
| --- | --- | --- |
| Transport | gRPC over a Unix socket (mode 0600) named in `RT_PLUGIN_SOCKET` | gRPC over TCP at `plugins.listen` |
| Encryption | none (local socket) | none: keep it on localhost, a private network or a VPN |
| Token | a new `rtm_…` token for each run, in `RT_PLUGIN_TOKEN`; it stops working when the process exits | an `rtp_…` token from **Plugins → Attach** or **New token** |

Every call carries the token as gRPC metadata: `authorization: Bearer <token>`. A missing or unknown
token fails with `UNAUTHENTICATED`. The token decides which plugin you are.

The server sends keepalive pings every 30 seconds. Clients may ping no more often than every 10
seconds.

## Managed and attached plugins

**Managed** plugins come in a bundle. RepeaterTastic starts `run.managed.exec` from the bundle
folder, in its own process group, while the plugin is enabled. It passes a small environment:

| Variable | Value |
| --- | --- |
| `RT_PLUGIN_ID` | the plugin id |
| `RT_PLUGIN_SOCKET` | the Unix socket to connect to |
| `RT_PLUGIN_TOKEN` | the token for this run |
| `RT_PLUGIN_DATA` | the plugin's data folder, `<plugins dir>/data/<id>` (also `Welcome.data_dir`) |
| `HOME` | the same data folder |
| `PATH`, `TZ`, `LANG`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | passed through when set |

- Anything the program prints to stdout or stderr goes to its log, one line at a time.
- A program that exits is restarted with backoff: 1 s, doubling up to a minute.
- After five exits within 15 seconds of starting, the plugin is left `crashed`.
- To stop the plugin, RepeaterTastic sends `Stop`, waits 5 seconds, sends SIGTERM to the process
  group, waits another 5, then sends SIGKILL.

**Attached** plugins run anywhere and connect over TCP. The SDK reads `RT_PLUGIN_ID`,
`RT_PLUGIN_ADDR` (`host:port`) and `RT_PLUGIN_TOKEN`.

- An attached plugin has no bundle. It should send its `plugin.yaml` in `Hello.manifest_yaml`, so
  the GUI can show its name, permissions and settings form.
- It gets no data folder (`Welcome.data_dir` is empty) and has to keep its own state.
- RepeaterTastic doesn't restart it. If a session is refused or ends, reconnect with a backoff.

## Session

`rpc Session(stream PluginMessage) returns (stream HostMessage)`

1. Open the stream and send `Hello` first.
2. The host answers `Welcome`, or ends the stream with an error.
3. The host then streams the events the plugin's permissions allow, on every radio.
4. The plugin sends `Status`, `LogLine`, `PanelData` and `Heartbeat` whenever it likes.
5. When the host sends `Stop`, close the stream and exit. Ending the stream means the plugin has
   stopped.

The other calls only work while a session is open. A new session with the same token replaces the
old one, which ends with `UNAVAILABLE` "replaced by a new session".

The host ends a session with `UNAVAILABLE` and a reason when:

- the plugin is stopped;
- its permissions change (it reconnects with the new grants);
- an attached plugin's token is replaced;
- a managed plugin's process exits.

### PluginMessage (plugin → host)

| Field | Message | Notes |
| --- | --- | --- |
| `hello` | `Hello` | must be the first message |
| `status` | `Status` | the plugin's card summary |
| `log` | `LogLine` | a line in the plugin's log |
| `heartbeat` | `Heartbeat` | empty; the host doesn't require it (the Go SDK sends one every 30 s) |
| `panel` | `PanelData` | data for the plugin's panel |

| Message | Field | Type | Meaning |
| --- | --- | --- | --- |
| `Hello` | `plugin_id` | string | must be the id the token belongs to |
| | `api_version` | uint32 | `1` |
| | `plugin_version` | string | shown in the log |
| | `manifest_yaml` | string | attached plugins: the whole `plugin.yaml`. Managed plugins leave it empty |
| `Status` | `summary` | string | one line on the plugin's card, e.g. "API connected" |
| | `fields` | map<string, string> | label: value pairs on the plugin's page; a status with more than 40 is ignored |
| | `state` | string | `ok`, `warning` or `error`; anything else shows as `ok` |
| `LogLine` | `level` | string | `debug`, `info`, `warn` or `error`; anything else is `info` |
| | `message` | string | cut at 2000 bytes |
| `PanelData` | `json` | string | valid JSON up to 1 MB; anything else is ignored with a warning in the log |

The plugin's log keeps its last 1000 lines. A new session clears the status and panel data.

### HostMessage (host → plugin)

| Field | Message | When |
| --- | --- | --- |
| `welcome` | `Welcome` | once, straight after `Hello` |
| `packet` | `PacketEvent` | a packet was received or sent (`packets.read`) |
| `node` | `NodeEvent` | a node's details changed (`nodes.read`) |
| `text` | `TextMessageEvent` | a relay persona text message (`messages.read`) |
| `traceroute` | `TracerouteEvent` | a traceroute reply (`traceroute.send` or `nodes.read`) |
| `settings` | `SettingsChanged` | the operator saved new settings |
| `stop` | `Stop` | the plugin should close the stream and exit |
| `action` | `PanelAction` | the plugin's panel posted an action |

| Message | Field | Type | Meaning |
| --- | --- | --- | --- |
| `Welcome` | `api_version` | uint32 | `1` |
| | `host_version` | string | the RepeaterTastic version |
| | `permissions` | repeated string | the permissions granted |
| | `settings_json` | string | the settings as a JSON object: defaults, then saved values, secrets included |
| | `radios` | repeated `Radio` | every radio on the site |
| | `data_dir` | string | a folder the plugin may write to (managed plugins; empty for attached) |
| `SettingsChanged` | `settings_json` | string | the whole settings object again |
| `Stop` | `reason` | string | e.g. "restarting", "permissions changed", "removed" |
| `PanelAction` | `name` | string | the action's name (1-100 characters) |
| | `payload_json` | string | the action's payload as JSON (`null` when none) |

Events go into a buffer of 2048 messages. A plugin that reads too slowly loses events rather than
holding up the radios; the number lost shows as `dropped_events` on its page.

## Calls

| RPC | Permission | Request → response |
| --- | --- | --- |
| `ListRadios` | none | `ListRadiosRequest {}` → `ListRadiosResponse {repeated Radio radios}` |
| `ListNodes` | `nodes.read` | `ListNodesRequest {radio_id}` → `ListNodesResponse {repeated Node nodes}` |
| `SendText` | `messages.send` | `SendTextRequest` → `SendResponse {packet_id}` |
| `Traceroute` | `traceroute.send` | `TracerouteRequest` → `SendResponse {}` |
| `ListSensors` | `sensors.publish` | `ListSensorsRequest {}` → `ListSensorsResponse {repeated Sensor sensors}` |
| `PublishSensor` | `sensors.publish` | `PublishSensorRequest {sensor_id, map<string,double> fields}` → `PublishSensorResponse {}` |

Every call needs an open session (`FAILED_PRECONDITION` "open the Session stream first") and its
permission (`PERMISSION_DENIED`).

### ListRadios

Every radio, each with its relay persona and identities. It's the same as `Welcome.radios`, but
current.

### ListNodes

`radio_id` picks one radio's node database; `""` returns every radio's nodes. A node heard by two
radios appears once per radio. An unknown `radio_id` returns an empty list.

The node database is shared by all of a radio's identities. It can hold names, positions and
metrics learned on channels or in DMs the relay persona can't read.

### SendText

Sends a text message **from the radio's relay persona**.

| Field | Type | Meaning |
| --- | --- | --- |
| `radio_id` | string | `""` = the main radio |
| `to` | string | `"!a1c40e07"` for a DM, `""` for a broadcast on `channel` |
| `channel` | uint32 | the channel index on the relay persona (use 0 for a DM) |
| `text` | string | 1-200 bytes |
| `want_ack` | bool | ask for an ACK |

`packet_id` in the reply is the packet's id. The message goes through the normal transmit queue and
duty cycle, and shows in the relay persona's chat in the GUI.

| Code | When |
| --- | --- |
| `NOT_FOUND` | no such radio |
| `INVALID_ARGUMENT` | `to` isn't a node id, `text` is empty or over 200 bytes, or the send is refused |
| `FAILED_PRECONDITION` | the radio is in `monitor` or `off` mode ("isn't transmitting") |
| `PERMISSION_DENIED` | `plugins.messages_per_hour` is 0 |
| `RESOURCE_EXHAUSTED` | the send budget is used up; the message says when to try again |

### Traceroute

Sends a traceroute from **the identity the operator chose** for the plugin on that radio.

| Field | Type | Meaning |
| --- | --- | --- |
| `radio_id` | string | `""` = the main radio |
| `target` | string | the node id to trace (not broadcast) |
| `from` | string | `""` = the chosen identity; anything else must be that identity |

- **Which identity sends:** the identities chosen in the plugin's `identities` settings that are on
  that radio. With none chosen there, the radio's relay persona sends. With several chosen, the first
  sends by default, and `from` may name any of them. A `from` naming any other identity, including
  the relay persona when identities are chosen, fails with `PERMISSION_DENIED`. The operator decides
  who transmits.
- **Result:** the reply arrives as a `TracerouteEvent`. The response has no packet id.

| Code | When |
| --- | --- |
| `NOT_FOUND` | no such radio |
| `INVALID_ARGUMENT` | `target` or `from` isn't a node id, or `target` is broadcast |
| `PERMISSION_DENIED` | `from` isn't the chosen identity, or `plugins.traceroutes_per_hour` is 0 |
| `FAILED_PRECONDITION` | the radio is in `monitor` or `off` mode |
| `RESOURCE_EXHAUSTED` | the send budget is used up, or the radio refused it: one traceroute per identity every 30 seconds |

### ListSensors and PublishSensor

The host's sensors, and readings for the ones the operator made for pushing ([Sensors](sensors.md)).

`Sensor {id, name, kind, map<string,double> fields, read_at_unix}` — `kind` is `push`, `exec` or
`file`, and `fields` is its latest reading.

`PublishSensor` gives a reading to a sensor whose `kind` is `push`. Field names are the Meshtastic
ones — `temperature`, `humidity`, `lux`, `voltage`, `current`, `pm10`, `pm25`, `pm100`, `distance`,
`radiation`, `rainfall_1h`, `rainfall_24h` — and any other name is ignored.

Publishing transmits nothing, so it costs no send budget: the identities the operator attached the
sensor to broadcast it themselves, on their own telemetry schedule, as their own sensor. A plugin
can't create a sensor or choose who publishes it — that stays the operator's decision.

| Code | When |
| --- | --- |
| `INVALID_ARGUMENT` | no such sensor, its kind isn't `push`, or no usable field was given |
| `UNAVAILABLE` | this host has no sensors at all |

The Go SDK wraps it: `c.PublishSensor(ctx, "weather", map[string]float64{"temperature": 18.4})`.

### Send budget

Each plugin has its own budget for `SendText` and for `Traceroute`. The operator sets it under
**Plugins → Send limits** (`plugins.messages_per_hour`, default 30, 0-600; `traceroutes_per_hour`,
default 12, 0-120).

- A plugin may send a sixth of its hourly budget at once (at least one).
- The budget then refills evenly. With 12 traceroutes an hour, that's 2 straight away and then one
  every 5 minutes.
- `0` refuses every send with `PERMISSION_DENIED`.
- Arguments and monitor or off mode are checked first, and a send the radio refuses (for example
  its limit of one traceroute per identity every 30 seconds) is given back, so refusals don't use
  the budget.
- Every send is written to the plugin's log.

## Events

A plugin receives events from every radio. Each carries `radio_id`. What arrives depends on the
permissions granted when the session opened.

### PacketEvent (`packets.read`)

One packet a radio received or transmitted.

| Field | Type | Meaning |
| --- | --- | --- |
| `radio_id` | string | the radio |
| `direction` | string | `rx` or `tx` |
| `kind` | string | how RepeaterTastic handled it (below) |
| `mesh_packet` | bytes | a `meshtastic.MeshPacket` protobuf (below) |
| `decoded` | bool | the payload is decoded |
| `channel_hash` | uint32 | the on-air channel hash (0 for a PKI DM) |
| `channel_name` | string | the channel it decoded on, `PKI` for a DM, `""` when undecoded |
| `time_ms` | int64 | when, in Unix milliseconds |
| `reporter_node_num` | uint32 | the radio's relay persona |
| `relay_channel_index` | int32 | the channel's index (0-7) on the relay persona, or `-1` (below) |
| `holders` | repeated `ChannelHolder` | identities that would hear it as a node does (below) |

| `direction` | `kind` |
| --- | --- |
| `rx` | `heard` (decoded, not for our identities), `delivered` (to one of our identities), `relayed`, `dup` (seen before), `echo` (our own packet repeated back), `legacy` (pre-2.3 firmware, ignored), `undecryptable`, `bad` |
| `tx` | `ours` (sent by one of our identities), `relayed` |

**`mesh_packet`**

- The payload is decoded (the `decoded` variant) when any channel or PKI key on that radio could read
  it. Otherwise it is encrypted exactly as heard.
- `pki_encrypted` is true for a PKI DM.
- `rx_time`, `rx_snr`, `rx_rssi`, `hop_limit`, `hop_start`, `via_mqtt`, `relay_node` and `next_hop` are
  filled. `rx_time` is set on every received packet.
- `channel` is `relay_channel_index` when that is set, and otherwise the on-air hash.

**`holders`** (`ChannelHolder {node_num, channel_index}`) lists every identity on the radio that
would hear the packet as a node does: those holding the channel, with the channel's index on each,
or the recipient of a DM (index 0). It is empty when the packet wasn't decoded.

**`relay_channel_index`** is the index on the relay persona when the relay persona is one of the
holders: it holds the channel, or the DM is addressed to it. It is `-1` otherwise.

Decoding uses every identity's channels and keys, not just the relay persona's. A plugin that
reports what a node hears should keep only what that node would hear itself:

- **as the relay persona:** `relay_channel_index >= 0`;
- **as another identity:** the identity is in `holders` (use its `channel_index` as the channel),
  and `to` is broadcast or that identity.

`ListRadios` lists each radio's identities.

### NodeEvent (`nodes.read`)

`NodeEvent {Node node}`: a node in a radio's node database changed, for example a new NodeInfo or
position.

### TextMessageEvent (`messages.read`)

A text message the radio's relay persona received or sent, once each. Sent messages arrive once,
when queued (including the plugin's own and those sent from the GUI). Messages to other identities
aren't included.

| Field | Type | Meaning |
| --- | --- | --- |
| `radio_id` | string | the radio |
| `identity_node_id` | string | the relay persona |
| `from`, `to` | uint32 | node numbers; `to` is `0xffffffff` for a broadcast |
| `channel` | uint32 | the channel index on the relay persona; 0 for DMs |
| `direct` | bool | a DM |
| `text` | string | the text |
| `packet_id` | uint32 | the packet id |
| `time_ms` | int64 | Unix milliseconds |
| `direction` | string | `in` or `out` |

### TracerouteEvent (`traceroute.send` or `nodes.read`)

A traceroute reply to the identity the plugin sends traceroutes from on that radio: its chosen
identities, or the relay persona when none is chosen. Replies to traceroutes sent from the GUI by
that identity arrive too. Timeouts aren't sent.

| Field | Type | Meaning |
| --- | --- | --- |
| `radio_id` | string | the radio |
| `identity_node_id` | string | the identity that sent the traceroute |
| `target_node_id` | string | the node traced |
| `route`, `snr_towards` | repeated string, repeated double | hops towards the target and their SNR |
| `route_back`, `snr_back` | repeated string, repeated double | hops back and their SNR |

## Model messages

**Radio**

| Field | Type | Meaning |
| --- | --- | --- |
| `id` | string | `main`, `mf`, … |
| `name` | string | display name |
| `region` | string | `EU_868` |
| `preset`, `preset_name` | string | `LONG_FAST`, `LongFast` |
| `frequency_mhz` | double | the radio's frequency |
| `connected` | bool | the modem is up and has accepted Meshtastic's radio settings |
| `relay` | `Identity` | the radio's relay persona |
| `identities` | repeated `Identity` | every identity on the radio, the relay persona first |

**Identity:** `node_id` (`"!a1c40e07"`), `node_num`, `long_name`, `short_name`, `radio_id`.

**Node**

| Field | Type | Meaning |
| --- | --- | --- |
| `node_num`, `node_id` | uint32, string | the node |
| `radio_id` | string | whose node database this is |
| `user` | bytes | `meshtastic.User` protobuf, when known |
| `position` | bytes | `meshtastic.Position` protobuf, when known |
| `device_metrics` | bytes | `meshtastic.DeviceMetrics` protobuf, when known |
| `last_heard_ms` | int64 | 0 when never heard |
| `snr`, `rssi` | float, int32 | the last signal report |
| `hops_away` | int32 | -1 when unknown |
| `via_mqtt` | bool | heard through MQTT |
| `local` | bool | one of RepeaterTastic's own identities |

## Errors

| Code | Where |
| --- | --- |
| `UNAUTHENTICATED` | missing or unknown token |
| `INVALID_ARGUMENT` | the first session message isn't `Hello`; `manifest_yaml` doesn't validate or its `id` differs; bad call arguments |
| `PERMISSION_DENIED` | `Hello.plugin_id` isn't the token's plugin; a permission isn't granted; traceroute `from` isn't allowed; a send limit is 0 |
| `FAILED_PRECONDITION` | `api_version` isn't 1; the plugin can't run yet (disabled, needs settings, needs a permission review); no session open; the radio is in monitor or off mode |
| `NOT_FOUND` | unknown `radio_id` in `SendText` or `Traceroute` |
| `RESOURCE_EXHAUSTED` | send budget used up; the radio's own traceroute rate limit |
| `UNAVAILABLE` | the host ended the session (the message says why) |
| `INTERNAL` | a bug in the host; the daemon keeps running |

When a session is refused because the plugin can't run, the message says why, for example "the
plugin can't run: needs_settings Fill in API key". Attached plugins hit this until the operator has
filled in required settings and reviewed any permissions asked for beyond the ones granted at attach.

## Manifest (`plugin.yaml`)

The manifest sits at the root of a bundle. Attached plugins send it in `Hello.manifest_yaml`.

| Field | Required | Meaning |
| --- | --- | --- |
| `id` | yes | 2-40 lowercase letters, digits and dashes, starting with a letter or digit. Never change it: installing a bundle with the same id upgrades the plugin |
| `name` | | display name (default: the id) |
| `version` | | the plugin's version |
| `api` | yes | Plugin API version: `1` |
| `description`, `author`, `homepage`, `license` | | shown in the GUI |
| `logo` | | bundle path to a PNG, SVG or WebP file |
| `permissions` | | the permissions it asks for (below) |
| `network` | | hosts the plugin talks to, shown before enabling. A declaration, not a firewall |
| `settings` | | the settings form (below) |
| `run.managed.exec` | managed plugins | bundle path to the program. `{os}` and `{arch}` become Go's `GOOS` and `GOARCH`, e.g. `bin/hello-{os}-{arch}` (`arm` for 32-bit Pis) |
| `run.managed.args` | | arguments for the program |
| `ui.panel` | | bundle path to an HTML file shown in a sandboxed frame on the plugin's page |

Paths must be clean relative paths inside the bundle. A bundle is refused when a named file is
missing or it has no program for the host's OS and CPU. A bundle is also refused when:

- it has paths outside the bundle, links or device files;
- it is over 100 MB zipped, over 250 MB unpacked, or has more than 2000 files.

### Permissions

| Permission | Lets the plugin |
| --- | --- |
| `packets.read` | receive `PacketEvent`s: every packet the radios hear and send, decoded where RepeaterTastic can read it |
| `nodes.read` | call `ListNodes`, and receive `NodeEvent`s and `TracerouteEvent`s |
| `messages.read` | receive `TextMessageEvent`s: text messages to and from each radio's relay persona |
| `messages.send` | call `SendText` from each radio's relay persona |
| `traceroute.send` | call `Traceroute` from the identity chosen in the plugin's settings (or the radio's relay persona), and receive `TracerouteEvent`s |
| `status.read` | call `GetStatus`: airtime, noise floor, channel use and the packet counters |
| `sensors.publish` | call `ListSensors` and `PublishSensor`: give readings to the host's push sensors, which identities publish as their own |

The operator may untick any of them. Check `Welcome.permissions` and cope with a missing one. A
new version that asks for more waits for the operator to review them.

### Settings

```yaml
settings:
  - key: api_key
    label: API key
    type: secret
    required: true
    help: From your account page.
  - key: report_as
    label: Report as
    type: identities
    placeholder: The relay persona
```

| Field | Meaning |
| --- | --- |
| `key` | 1-40 lowercase letters, digits and underscores, starting with a letter; unique |
| `label` | the form label (default: the key) |
| `type` | one of the types below (default `string`) |
| `help` | a line under the field |
| `required` | the plugin can't be enabled or run until it has a value |
| `default` | used until the operator saves a value; included in `settings_json` |
| `options` | the choices for `select` and `multiselect` (required for them) |
| `placeholder` | the empty field's hint; for list types, what an empty list shows as |

| Type | GUI | JSON value |
| --- | --- | --- |
| `string` | text box | string, trimmed, up to 4096 bytes |
| `secret` | password box; never shown again | string |
| `url` | text box | an `http`, `https`, `ws` or `wss` URL |
| `bool` | switch | `true` or `false` |
| `int` | number box | a whole number |
| `number` | number box | a number |
| `select` | drop-down of `options` | one of `options` |
| `multiselect` | tick-box list of `options` | a list of `options` |
| `radios` | tick-box list of the site's radios | a list of radio ids. Empty usually means every radio |
| `identities` | tick-box list of the site's identities | a list of node ids, e.g. `["!a1c40e07"]`. It chooses who `Traceroute` sends from |
| `nodes` | searchable tick-box list of the nodes the site has heard, most recent first | a list of node ids, e.g. `["!a1c40e07"]`, of nodes out on the mesh |

- An empty string or empty list removes the value, so the default applies again.
- Secrets reach the plugin in full, but the API and GUI only show that one is saved.
- Settings pinned in `plugins.entries` expand `${VAR}` from the daemon's environment.
- A running plugin gets `SettingsChanged` with the whole object when the operator saves.

## Panels

A plugin with `ui.panel` gets a page in the GUI, shown in a sandboxed frame. The panel is served
with `Content-Security-Policy: sandbox allow-scripts allow-popups` and `connect-src 'none'`. It runs
in an opaque origin: it can't make network requests, or read the GUI's storage, login or API.
Other files in the panel's folder (scripts, styles, images) are served beside it.

It talks to the GUI with `postMessage`:

| Direction | Message |
| --- | --- |
| GUI → panel | `{type: "data", data, theme: "light" \| "dark"}`: the plugin's latest `PanelData`, sent when it changes and when the panel loads |
| panel → GUI | `{type: "ready"}`: ask for the data now |
| panel → GUI | `{type: "action", name, payload}`: delivered to the plugin as `PanelAction` |
| panel → GUI | `{type: "resize", height}`: set the frame height in pixels (120-4000) |

Build the panel from `data` with DOM methods, not `innerHTML`: packet contents come from the mesh.

## Go SDK

```go
c, err := sdk.Connect(ctx, sdk.Options{Version: "1.0.0"}) // reads RT_PLUGIN_* from the environment
if err != nil { log.Fatal(err) }
defer c.Close()
_ = c.Status("connected", "ok", nil)
for msg := range c.Events() {
    if t := msg.GetText(); t != nil && t.Direction == "in" && t.Text == "ping" {
        req := &pluginv1.SendTextRequest{RadioId: t.RadioId, Channel: t.Channel, Text: "pong"}
        if t.Direct {
            req.To, req.Channel = fmt.Sprintf("!%08x", t.From), 0 // answer a DM with a DM
        }
        c.Host.SendText(c.Context(), req)
    }
}
```

| | |
| --- | --- |
| `Options` | `ID`, `Version`, `Socket`, `Addr`, `Token`, `ManifestYAML`. Empty fields come from `RT_PLUGIN_ID`, `RT_PLUGIN_SOCKET`, `RT_PLUGIN_ADDR` and `RT_PLUGIN_TOKEN`; `Socket` wins over `Addr` |
| `Connect(ctx, o)` | opens the session, sends `Hello` and waits for `Welcome`. `ctx` is the session's lifetime: don't give it a timeout |
| `c.Welcome` | the `Welcome` message |
| `c.Host` | the `PluginHostClient` for `ListRadios`, `ListNodes`, `SendText` and `Traceroute` |
| `c.Events()` | a channel of `HostMessage`s after `Welcome`; closes when the session ends |
| `c.Context()` | ends with the session |
| `c.Err()` | why the session ended, once `Events` has closed |
| `c.Settings(&v)` | decodes the current settings (kept up to date by `SettingsChanged`) |
| `c.Status(summary, state, fields)`, `c.Log(level, format, args...)`, `c.Panel(v)` | send `Status`, `LogLine` and `PanelData` |
| `c.Close()` | ends the session |

The SDK sends a `Heartbeat` every 30 seconds. [`examples/plugins/hello`](../examples/plugins/hello)
is a complete plugin with a panel; `make plugin-example` builds its bundle.
