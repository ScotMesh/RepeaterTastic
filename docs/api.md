# RepeaterTastic HTTP API (v1)

[← README](../README.md) · [Hardware](hardware.md) · [Configuration](configuration.md) · [Web GUI](web-gui.md) · [Several radios](radios.md) · [MQTT](mqtt.md) · [Plugins](plugins.md) · [Architecture](architecture.md)

The web GUI uses this API, and so can scripts and integrations such as Home Assistant.

- **Base URL:** `http://<host>:8080/api/v1` (`web.bind` and `web.port`). The GUI is served at `/`.
- **Format:** JSON in and out. Request bodies are limited to 1 MB, except plugin uploads (100 MB)
  and backup restores (32 MB).
- **Errors:** `{"error": "human readable message"}` with a 4xx or 5xx status. A path that doesn't
  exist under `/api/` answers 404 `{"error": "no such endpoint"}`.
- **Auth:** `Authorization: Bearer <token>` on every call except those in [Setup and auth](#setup-and-auth).
- **Conventions:** times are Unix **milliseconds** unless a name says otherwise (`uptime_s`,
  `bucket_s`). Node ids are strings like `"!a1c40e07"`; `node_num` is the same value as a number.

## Contents

- [Setup and auth](#setup-and-auth)
- [Radios and the `?radio=` parameter](#radios-and-the-radio-parameter)
- [Status and relay](#status-and-relay)
- [Restart](#restart)
- [Identities](#identities)
- [Channels](#channels)
- [Messages (browser chat)](#messages-browser-chat)
- [Nodes](#nodes)
- [Packets](#packets)
- [Live events (SSE)](#live-events-sse)
- [Statistics](#statistics)
- [Configuration](#configuration)
- [Links](#links)
- [API tokens, logs, backup and restore](#api-tokens-logs-backup-and-restore)
- [meshtasticd nodes](#meshtasticd-nodes)
- [Sensors](#sensors)
- [Plugins](#plugins)

## Setup and auth

### Tokens

| Token | Where it comes from | Lifetime |
| --- | --- | --- |
| Session (JWT) | `POST /auth/login`, `POST /setup`, `PUT /auth/password` | `web.session_ttl` (default 7 days). Changing the password or `POST /auth/logout-all` ends every session |
| API token (`rpt_…`) | `POST /tokens` | Until deleted. Not affected by password changes or logout-all |

Send either as `Authorization: Bearer <token>`. A missing or bad token answers 401
`{"error": "unauthorized"}`. `GET /events` also accepts `?token=<token>`, because the browser's
EventSource can't set headers. No other endpoint reads a token from the URL.

### Endpoints

| Method and path | Auth | Body → response |
| --- | --- | --- |
| `GET /setup` | none | → `{"needed": true}` while no admin password exists |
| `POST /setup` | none | `{"password", "region", "preset", "primary_channel", "driver", "device", "relay_role", "hosted"}` → `{"token", "expires", "restart_required"}` |
| `POST /auth/login` | none | `{"password"}` → `{"token", "expires"}` |
| `PUT /auth/password` | token | `{"current", "new"}` → `{"token", "expires"}` |
| `POST /auth/logout-all` | token | → 204 |
| `GET /serial-ports` | setup | → `[{"path", "description", "device"}]` |
| `GET /boards` | setup | → `[{"id", "name", "module", "bus", "source", "supported", "error"}]` |
| `GET /regions` | setup | → `[{"name", "presets": ["LONG_FAST", …], "duty_cycle_pct", "power_limit_dbm", "start_mhz", "end_mhz"}]` |
| `POST /phy/preview` | setup | `{"region", "preset", "primary_channel", "tx_power_dbm"}` → a [`phy`](#status-and-relay) object |
| `POST /setup/probe` | setup | `{"device", "driver"}` → `{"ok", "driver", "firmware", "name", "sync_word_ok", "error", "details"}` |
| `POST /setup/meshtasticd` | setup | `{"meshtasticd", "docker_image"}` → `{"ok", "version", "min_version", "launcher", "error"}` |

"Setup" means no token is needed while `GET /setup` reports `needed: true`; after that a token is.

- `POST /setup` answers 409 once setup is done, and 400 when the password is under 8 characters or
  the settings don't validate. Nothing is saved on a 400, so setup can be tried again. Empty fields
  keep their defaults; `primary_channel` `""` means the preset's name. `driver` is `kiss` (a USB
  modem on the serial port `device`), `spi` (a LoRa board: `device` is a board id from
  `GET /boards`) or `meshtastic` (a board on Meshtastic firmware: `device` is its serial port or
  `host[:port]`; switching to it needs a restart); without `driver`, `device` is taken as a serial port and left alone when the
  config already has an `spi` radio. A modem that hasn't opened yet switches to the chosen
  `device` at once, so `restart_required` is normally false; a change of driver needs a restart.
- `GET /boards` lists the boards the experimental `spi` driver knows: `auto` first (detect a
  CH341 stick, a Pi HAT+ or a RAK EEPROM), then meshtasticd's own board files in
  `/etc/meshtasticd/config.d` and `available.d`, then the built-in copies by name. `id` is what
  to put in `radio.device` (a file path, or a built-in file name such as
  `lora-MeshAdv-900M30S.yaml`); `bus` is `spidev0.0` or `usb`; `source` is `auto`, `config.d`,
  `available.d` or `built-in`; `supported: false` with `error` marks a board file this driver
  can't use yet.
- `POST /auth/login` answers 401 `wrong password`. After 5 failures from one address, it answers
  429 for a minute.
- `PUT /auth/password` answers 400 when `current` is wrong or `new` is under 8 characters. Every
  other session is signed out; the returned token keeps this one signed in.
- `POST /setup/probe` always answers 200. It pings the modem, reads its version and checks it
  accepts Meshtastic's sync word. `ok: false` and `error` explain a failure. An empty `device`, or
  the running modem's own device, reports the running modem without opening the port again. A
  device that isn't a serial port answers 400. With `driver: "spi"` it resolves `device` (a board
  id from `GET /boards`), opens the board, reads the chip and closes it again: `firmware` is the
  LoRa module, `name` the board, `sync_word_ok` true, and `details` the chip's diagnostic lines
  (the SX126x/SX127x/SX128x/LR11x0 version, mode and error flags). A board a running radio
  already drives is reported without being opened. A board file outside `/etc/meshtasticd`
  answers 400.
- With `driver: "auto"` the probe finds out what is on a serial port: it pings for a KISS modem
  first (Meshtastic firmware ignores that), then tries the Meshtastic client API. The answer is
  that probe's, with `driver` `kiss` or `meshtastic`; when neither answers, `driver` is empty and
  `details` has both errors. A port a running radio uses is reported without opening it. The setup
  wizard runs this when a listed port is picked.
- With `driver: "meshtastic"` the probe connects to the board, reads its settings and disconnects:
  `name` is its long name, `firmware` its Meshtastic version, `region`, `preset`, `role` and
  `node_id` its current settings, and `details` a summary. A board a running radio uses is
  reported without a second connection. Before a password exists, a network address must be on
  this machine or the local network (400 otherwise).
- `POST /phy/preview` answers 400 for an unknown preset or region. `tx_power_dbm` is clamped to the
  region limit in the reply.

## Radios and the `?radio=` parameter

A host can run several radios ([Several radios](radios.md)). Radio `main` is the top-level radio.

- **`?radio=<id>`** picks the radio for endpoints about one radio: status, relay, identity list and
  create, preview-key, nodes, node actions, packets, events, statistics, config and links. Without
  it, they use the main radio. An unknown id also falls back to the main radio.
- **`?radio=all`** covers every radio of the site: `GET /identities`, `/nodes`, `/packets`, `/links`,
  `/events` and the `/stats/…` endpoints. The GUI uses it everywhere, filtering by radio itself.
  - Packets carry `radio_id`, and are merged newest first. Links carry `radio_id` and `radio_name`.
    Identity statistics carry `radio_id`.
  - Nodes are merged: each is shown as its most recent sighting, with `heard_by` listing every radio
    that heard it (empty for the site's own identities). A single-radio `GET /nodes` has
    `heard_by` too.
  - Airtime buckets are summed over the radios, and RF points combined.
  - `GET /events?radio=all` sends a `status` event for each radio every few seconds (`radio_id` says
    which), packets with `radio_id`, and node events merged as above.
- Endpoints under `/identities/{node_id}/…` and `/nodes/{node_id}/…` find the identity's radio
  themselves.

| Method and path | Body → response |
| --- | --- |
| `GET /radios` | → `{"radios": [Radio], "site", "pending", "restart_required"}` |
| `POST /radios` | `{"id", "name", "driver", "device", "region", "preset", "primary_channel", "tx_power_dbm", "relay_role", "copy_position"}` → 201 `{"id", "restart_required": true}` |
| `PATCH /radios/{id}` | `{"name"}` → `{"id", "name"}` |
| `PUT /radios/{id}` | `{"name", "driver", "device", "region", "preset", "tx_power_dbm", "relay_role"}` → `{"id", "restart_required": true}` |
| `DELETE /radios/{id}` | → `{"id", "restart_required"}` |
| `GET /site` | → `{"duty_cycle_percent", "main_radio_name", "running_duty_cycle_percent", "coordinator"}` |
| `PUT /site` | `{"duty_cycle_percent"}` → the same, plus `restart_required` |

A Radio in the list:

```json
{"id": "main", "name": "Main", "main": true, "device": "/dev/ttyUSB0", "driver": "kiss",
 "firmware": "Mesh KISS v2", "connected": true, "configured": true, "noise_floor_dbm": -111,
 "phy": {"…": "as status.phy"}, "relay": {"role": "client_mute", "node_id": "!be77562b", "long_name": "Relay"},
 "identities": 3, "tx_pct": 0.1, "channel_util_pct": 6.2, "overlaps": ["mf"]}
```

- `identities` counts identities other than the relay persona. `overlaps` lists radios whose channel
  overlaps this one's; they take turns to transmit.
- `site` is `{"radios", "duty_limit_pct", "tx_pct"}`, or `null` for a single radio without a site
  airtime cap.
- `pending` lists radios that differ between the config and what's running:
  `{"id", "name", "device", "driver", "region", "preset", "tx_power_dbm", "relay_role", "action": "start"}`
  for an added radio, `{"id", "name", "device", "action": "remove"}` for a removed one.
- `POST /radios` writes the radio to `radios:`; it starts at the next restart. `driver` defaults
  to `kiss` (`device` a serial port); with `spi` (experimental), `device` is a board id from
  `GET /boards`. `preset` defaults to `LONG_FAST`, and `copy_position` (default true) copies the main site
  position. 400 when the id is missing or the config doesn't validate.
- `PATCH /radios/{id}` renames any radio, the main one included (saved as `site.main_radio_name`).
  Names are at most 40 characters; an empty name goes back to the default. 404 for an unknown radio.
- `PUT /radios/{id}` edits a radio that was added but hasn't started. A running radio answers 409;
  edit it with `PUT /config?radio=<id>` instead.
- `DELETE /radios/{id}` removes an extra radio from the config; it stops at the next restart. Its
  state folder is kept, so adding the same id back brings its identities back. The main radio
  answers 400.
- `PUT /site` sets the site airtime cap (0-100, 0 = none). It applies live when a site
  coordinator is running (several radios, or a cap set at start). Turning a cap on for a single
  radio needs a restart.

## Status and relay

`GET /status[?radio=<id>]`

```json
{
  "version": "0.2.0", "uptime_s": 5234, "radio_id": "main", "radio_name": "Main", "site": null,
  "radio": {"driver": "kiss", "device": "/dev/ttyUSB0", "firmware": "Mesh KISS v2", "name": "Heltec V3",
            "connected": true, "configured": true, "reconnects": 0, "rx": 1203, "tx": 311, "errors": 2,
            "noise_floor_dbm": -118, "queue": 0},
  "phy": {"region": "EU_868", "preset": "LONG_FAST", "preset_name": "LongFast", "frequency_mhz": 869.525,
          "bw_khz": 250, "sf": 11, "cr": 5, "slot": 0, "num_slots": 1, "sync_word": 43, "preamble": 16,
          "tx_power_dbm": 27, "primary_channel": "LongFast", "duty_cycle_pct": 10},
  "relay": {"role": "client", "node_id": "!3f0a91c2", "node_num": 1057657282,
            "long_name": "RepeaterTastic Relay", "short_name": "RPTR"},
  "map": {"tile_url": "https://…/{z}/{x}/{y}.png"},
  "restart_reasons": [],
  "nodes": {"state": "ok", "nodes": 4, "up": 4, "problems": [], "version": "2.8.0.47db0e3-alpha",
            "launcher": "meshtasticd"},
  "airtime": {"window_s": 3600, "tx_ms": 147700, "rx_ms": 402000, "duty_limit_pct": 10, "tx_pct": 4.1,
              "channel_util_pct": 11.2},
  "counters": {"rx": 1203, "rx_dupe": 402, "rx_undecryptable": 77, "rx_bad": 0, "tx": 311, "tx_failed": 0,
               "relayed": 120, "relay_cancelled": 33, "ack_ok": 41, "ack_fail": 3, "dropped_duty": 0}
}
```

- `radio.configured` is true once the modem has accepted Meshtastic's sync word and radio settings.
- `airtime.duty_limit_pct` is 100 with `override_duty_cycle`.
- `restart_reasons` lists saved changes waiting for a restart, as short labels: `"restored backup"`,
  `"web address"`, `"mDNS"`, `"site airtime cap"`, `"plugins"`, `"<radio> added"`,
  `"<radio> removed"`, `"<radio> modem connection"`, `"<radio> MQTT"` and `"<radio> UDP multicast"`.
  The same list decides every `restart_required` in this API.
- `nodes` is how meshtasticd is doing: the worst state across the site's radios ([meshtasticd
  nodes](#meshtasticd-nodes)). `state` is `ok`, `starting` (within a 90 s grace period after a
  start), `warning` (one or more identities down past the grace period) or `error` (meshtasticd
  can't run and nothing is up, every node is down, or a relay persona is down). `nodes` and `up`
  count instances; `problems` lists what's wrong, prefixed with the radio's name when there's more
  than one; `launcher` is `meshtasticd` or `docker <image>`.

### Relay role

`PUT /relay[?radio=<id>]` `{"role": "client" | "client_base" | "client_mute" | "router" | "router_late" | "monitor" | "off"}` → `status.relay`

The roles are Meshtastic's device roles plus two radio modes. `relay.favorites` in `PUT /config`
(node IDs) are the favourites of a `client_base` relay; the host's identities always count, and a
relay on meshtasticd or a board gets them as Meshtastic favourites (added as contacts first when
it hasn't heard of them). `"mute"` is still accepted and read as
`client_mute`. The rebroadcast mode is `relay.rebroadcast` in `PUT /config`
([Configuration](configuration.md#relay-the-relay-persona)); `GET /config` reports `"all"` when unset.

| Role | Relay persona | Radio |
| --- | --- | --- |
| `client` | Repeats like a normal node: after routers, and cancels if another node relays first | Sends and receives |
| `client_base` | A client that repeats for its favourited nodes with router priority | Sends and receives |
| `client_mute` | Never repeats | Sends and receives; identities still send |
| `router` | Always repeats, first | Sends and receives |
| `router_late` | Always repeats, after everyone else | Sends and receives |
| `monitor` | Never repeats | Listens only: nothing is transmitted (no messages, ACKs, NodeInfo or telemetry) |
| `off` | Never repeats | Ignored: nothing is received or sent |

Switching to `monitor` or `off` fails everything still queued. While the radio is in either mode,
sends fail with routing error `NO_INTERFACE`: a browser chat message is accepted (202) and comes back
with `"status": "failed", "error": "NO_INTERFACE"`, and a traceroute answers 429. DMs between
identities on the same host are still delivered locally. An unknown role answers 400. The change
is saved to the config and applies live. `relay.role` in `PUT /config` does the same.

## Restart

`POST /restart` → 202 `{"restarting": true}`. The daemon shuts down cleanly and exits with status
75, so its supervisor starts it again (systemd `Restart=on-failure`, a Docker restart policy).
Without a supervisor it stays stopped.

## Identities

`GET /identities[?radio=<id>|all]` → `[Identity]`

```json
{
  "node_id": "!a1c40e07", "node_num": 2713980423, "long_name": "Base Camp", "short_name": "BASE",
  "role": "CLIENT_MUTE", "hw_model": "HELTEC_V3", "public_key": "base64…", "is_relay": false,
  "real_node": true, "hosted": true, "enabled": true,
  "api": {"bind": "0.0.0.0", "port": 4403, "clients": 1, "listening": true},
  "outbox": 0, "airtime_ms_1h": 3200, "share_pct": 2.2, "share_limit_pct": 25, "created_at": 1757900000000,
  "last_byte": 7, "hop_limit": 0, "position": null, "position_secs": 0, "unread": 3,
  "radio_id": "main", "radio_name": "Main",
  "channels": [
    {"index": 0, "role": "PRIMARY", "name": "", "display_name": "LongFast", "psk": "AQ==", "hash": 8,
     "uplink": false, "downlink": false, "locked": true}
  ]
}
```

- The relay persona is included, with `"is_relay": true` and `"api": null`.
- `"real_node": true` marks an identity that is a real Meshtastic node (every identity, now): its
  names, channels and settings are written to it. `"hosted": true` means RepeaterTastic started its
  meshtasticd and keeps its key, so `GET …/key` answers as usual; it's `false` for the relay persona
  of a board radio, where the board keeps its own key.
- An identity's `role` must be `CLIENT_MUTE`, `TRACKER`, `SENSOR` or `TAK_TRACKER` (400 otherwise,
  on create and on `PATCH`). Creating, moving and deleting start and stop its meshtasticd.
- `share_pct` is this identity's part of the radio's transmit time in the last hour. `share_limit_pct`
  is its own limit, or `airtime.identity_share_percent`. `unread` counts unread browser-chat messages.
- `position` is the identity's own fixed position `{"latitude", "longitude", "altitude"}`, or `null`
  to use the radio's.

| Method and path | Body → response |
| --- | --- |
| `POST /identities[?radio=<id>]` | `{"long_name", "short_name", "private_key", "api_port", "api_bind", "role", "share_limit_pct", "hop_limit", "radio_id"}` → 201 Identity |
| `POST /identities/preview-key[?radio=<id>]` | `{"private_key"}` → `{"private_key", "public_key", "node_id", "node_num", "last_byte", "collision"}` |
| `PATCH /identities/{node_id}` | any of `{"long_name", "short_name", "enabled", "api_port", "api_bind", "role", "share_limit_pct", "hop_limit", "position", "position_secs", "app_settings"}` → Identity |
| `DELETE /identities/{node_id}` | → 204 |
| `GET /identities/{node_id}/key` | → `{"private_key", "public_key"}` (base64) |
| `POST /identities/{node_id}/move` | `{"radio_id"}` → Identity |
| `POST /identities/{node_id}/api/restart` | → 204 |

`app_settings` (default `false`) lets an app connected to the identity change its node's radio,
device, module and position settings, and reboot or reset it. Off, the identity's app port answers
such admin requests with a `NOT_AUTHORIZED` routing error; reading settings, and changing the names,
channels and node list (which RepeaterTastic keeps in step), still work.

- **Create:** `long_name` is required. `private_key` is an optional 32-byte base64 key; without it a
  key is generated whose last byte doesn't clash with a heard node. `api_port` 0 picks the next free
  port from 4403. `role` defaults to `CLIENT_MUTE`, so only the relay persona repeats. `radio_id`
  overrides `?radio=`. 409 when the key already exists on any radio or the port is taken; 400 for a
  bad key, role, hop limit or radio.
- **Preview key:** generates (or checks) a key before creating. `collision` is the node id of a
  local or heard node sharing the last byte, or `null`. Send the returned `private_key` to
  `POST /identities` so the node id matches the preview.
- **Patch:** a request is checked completely before anything changes.
  - `role` is a Meshtastic device role (`CLIENT`, `CLIENT_MUTE`, `CLIENT_HIDDEN`, `TRACKER`, `SENSOR`, …).
  - `hop_limit` is 0-7 (0 = the radio's) and caps every packet the identity sends, whatever its app asks.
  - `share_limit_pct` is 0-100 (0 = the site default).
  - `api_bind` is an IP address or `""` for every interface.
  - `position` takes `{"latitude", "longitude", "altitude"}`, or `null` to remove it.
  - `position_secs` is 0 (the radio's interval) or at least 1800.
  - Changing a name broadcasts a NodeInfo.
  - Answers 409 when `api_port` is taken, and 400 otherwise.
  - The relay persona ignores `role`, `enabled` and `api_port`.
- **Delete:** the relay persona answers 409.
- **Move** puts an identity on another radio with its key, node id, channels, settings, app port and
  chats. Its primary channel follows the new radio's preset. Unsent messages are marked failed. 409
  for a relay persona, or when another identity there shares its last byte.
- **API restart** restarts the identity's app server, which drops connected apps. The relay persona
  answers 409.

From the Meshtastic app, each identity also takes its device role, hop limit (as its cap), fixed
position and broadcast interval, owner name and channels. Radio-wide LoRa settings sent from an app
(region, preset, power, frequency) are ignored, since every identity shares the radio.

## Channels

| Method and path | Body → response |
| --- | --- |
| `PUT /identities/{node_id}/channels/{index}` | `{"name", "psk", "role", "uplink", "downlink"}` → Identity |
| `GET /identities/{node_id}/channels/url` | → `{"url": "https://meshtastic.org/e/#…"}` |
| `POST /identities/{node_id}/channels/url` | `{"url"}` → Identity |

- `index` is 0-7. `psk` is base64 (`AQ==` is the default key, `""` no key). `role` is `PRIMARY`,
  `SECONDARY` or `DISABLED`; `DISABLED` removes the channel. Names are at most 11 characters.
- Slot 0 is the radio's shared primary channel. Its role and name can't change (409); only its key
  can. The name is set by `mesh.primary_channel`.
- Importing a URL keeps existing channels: the URL's primary channel sets slot 0's key when its name
  matches, and the others go into free slots (extras are skipped). 400 when it isn't a channel URL.

## Messages (browser chat)

Every identity can chat from the browser, the relay persona included.

| Method and path | Body → response |
| --- | --- |
| `GET /identities/{node_id}/conversations` | → `[{"key", "title", "last_text", "last_time", "unread"}]` |
| `GET /identities/{node_id}/messages?conversation=&before=&limit=` | → `[Message]` |
| `POST /identities/{node_id}/messages` | `{"to", "channel", "text", "want_ack"}` → 202 Message |
| `POST /identities/{node_id}/conversations/{key}/read` | → 204 |

- A conversation `key` is `ch:<index>` or `dm:!<node id>`. Every enabled channel is listed, with
  `last_time: 0` and an empty `last_text` until something is said on it, so a new identity can post
  straight away. Conversations with history come first, newest first.
- `messages`: `limit` is 1-500 (default 50); `before` pages back by time; an empty `conversation`
  lists all of them.
- Send: `to` is a node id, or empty for a broadcast on `channel`. `want_ack` defaults to true. Text is
  1-200 bytes (400 otherwise). The reply is the queued Message; its status then changes over SSE.
- Mark read takes the key URL-encoded (`dm%3A!5b9e2213`) and sends an SSE `identity` event with the
  new `unread`.

```json
{"id": 195939341, "from": "!a1c40e07", "to": "!ffffffff", "channel": 0, "text": "hello",
 "time": 1757900000000, "direction": "out", "status": "queued", "error": "", "pki": false,
 "rssi": -92, "snr": 6.5, "hops": 1}
```

`direction` is `in` or `out`. `status` is `queued`, `sent`, `acked`, `failed` or `received`. `error`
is a Meshtastic routing error such as `NO_INTERFACE` or `MAX_RETRANSMIT`.

## Nodes

`GET /nodes[?radio=<id>|all]` → `[Node]`, the radio's shared node database (every radio's, merged, with `all`).

```json
{"node_id": "!5b9e2213", "node_num": 1537090067, "long_name": "Hilltop", "short_name": "HILL",
 "hw_model": "HELTEC_V3", "role": "ROUTER", "has_user": true, "has_public_key": true,
 "last_heard": 1757900000000, "snr": 7.25, "rssi": -88, "hops_away": 0, "next_hop": null,
 "via_mqtt": false, "local": false, "favorite": false, "ignored": false,
 "position": {"lat": 55.95, "lon": -3.19, "alt": 120, "time": 1757900000000},
 "telemetry": {"battery": 87, "voltage": 4.05, "channel_util": 12.3, "air_util_tx": 1.2},
 "known_by": ["!a1c40e07"]}
```

- A node heard without a NodeInfo gets the firmware's placeholders (`"Meshtastic 77f6"`, `"77f6"`,
  `UNSET`, `CLIENT`) and `"has_user": false`, so every node carries every field.
- `snr` and `rssi` are `null` unless the node was heard directly. `last_heard`, `hops_away`,
  `next_hop`, `position` and `telemetry` are `null` when unknown.
- `local` marks this host's own identities. `known_by` lists the enabled local identities that
  share this node database (empty for local nodes).

| Method and path | Body → response |
| --- | --- |
| `POST /nodes/{node_id}/traceroute[?radio=<id>]` | `{"from"}` → 202 `{"status": "sent"}` |
| `POST /nodes/{node_id}/request-nodeinfo[?radio=<id>]` | `{"from"}` → 202 `{"status": "sent"}` |
| `DELETE /nodes/{node_id}[?radio=<id>]` | → 204 |
| `GET /nodes/{node_id}/sightings` | → `[{"radio_id", "radio_name", "last_heard", "snr", "rssi", "hops_away", "via_mqtt"}]` |

- `from` is one of the radio's identities, or empty for its relay persona (400 otherwise).
- A traceroute answers 429 when it can't be sent: one per identity every 30 seconds, or
  `NO_INTERFACE` in monitor or off mode. The result arrives as an SSE `traceroute` event, or an event
  with `error` after 60 seconds without a reply.
- Deleting a local identity's node answers 409.
- `sightings` lists what every radio on this mast knows about a node, freshest first ([Several
  radios](radios.md)).

## Packets

`GET /packets[?radio=<id>|all]` → `[Packet]`, newest first.

| Query | Meaning |
| --- | --- |
| `limit` | 1-2000 (default 100) |
| `before` | only packets older than this time (paging) |
| `since` | only packets at or after this time |
| `node` | `from` or `to` is this node id |
| `port`, `kind`, `direction`, `channel` | exact match |
| `q` | case-insensitive text in `summary` |

```json
{"seq": 88123, "time": 1757900000000, "direction": "rx", "kind": "relayed", "id": 195939341,
 "from": "!5b9e2213", "to": "!ffffffff", "channel_hash": 8, "channel": "LongFast",
 "port": "TEXT_MESSAGE_APP", "hop_limit": 2, "hop_start": 3, "want_ack": false, "via_mqtt": false,
 "next_hop": 0, "relay_node": 19, "rssi": -91, "snr": 5.75, "size": 42, "airtime_ms": 612,
 "decoded_by": "!a1c40e07", "pki": false, "summary": "hello world", "payload": {"text": "hello world"},
 "raw": "hex of the full frame", "transport": "lora"}
```

| `direction` | `kind` |
| --- | --- |
| `rx` | `heard` (decoded, not for our identities), `delivered` (to one of our identities), `relayed`, `dup`, `echo` (our own packet repeated back), `legacy` (pre-2.3 firmware), `undecryptable`, `bad` |
| `tx` | `ours`, `relayed` |
| `local` | `local`: a DM between identities on this host, delivered without the radio |

`channel`, `port`, `decoded_by`, `summary`, `payload`, `raw` and `transport` are left out when empty.

## Live events (SSE)

`GET /events[?radio=<id>]` with `Authorization: Bearer` or `?token=` → `text/event-stream`, for one
radio. Each event is `event: <type>` and `data: <JSON>`.

| Event | When | Data |
| --- | --- | --- |
| `status` | on connect, then every 5 s | Status |
| `packet` | a packet is received or sent | Packet |
| `identity` | an identity changes (names, channels, unread, …) | Identity, or `{"node_id", "deleted": true}` |
| `message` | a chat message arrives or its status changes | `{"identity": "!a1c40e07", "message": Message}` |
| `node` | a node's details change | Node |
| `traceroute` | a traceroute reply, or a timeout | `{"identity", "target", "route", "snr_towards", "route_back", "snr_back"}`, plus `"error": "no response within 60 s"` on a timeout |
| `log` | a daemon log line (on every radio's stream) | `{"time", "level", "msg"}` |
| `plugin` | a plugin changes (on every radio's stream) | Plugin, or `{"id", "deleted": true}` |
| `sensor` | a sensor reads, fails or is edited (on every radio's stream) | Sensor, or `{"id", "deleted": true}` |

A slow client misses events rather than holding up the radio. Reload the lists after reconnecting.
The GUI closes the stream of a tab that has been in the background for 15 seconds, and reconnects
and reloads when the tab is shown again.

## Statistics

The `window` parameter is `1h`, `24h` (default) or `7d`.

| Method and path | Response |
| --- | --- |
| `GET /stats/airtime?window=` | `{"bucket_s": 600, "buckets": [{"time", "tx_ms", "rx_ms", "relay_ms", "by_identity": {"!a1c40e07": 120}}]}` |
| `GET /stats/ports?window=` | `[{"port": "TEXT_MESSAGE_APP", "rx": 120, "tx": 30}]`, busiest first |
| `GET /stats/rf?window=` | `{"bucket_s": 60, "points": [{"time", "noise_floor_dbm", "channel_util_pct", "rx", "tx"}]}` |
| `GET /stats/identities?window=` | `[{"node_id", "tx", "rx", "ack_ok", "ack_fail", "airtime_ms"}]` |

- `stats/airtime`: `tx_ms` is `relay_ms` plus every `by_identity` value.
- `stats/rf` is sampled every minute and kept for a week. Buckets are 60 s for `1h`, 600 s for `24h`
  and 3600 s for `7d`.
- `stats/identities` includes the relay persona, whose airtime includes relaying.
- All take `?radio=<id>`.

## Configuration

`GET /config[?radio=<id>]` → the configuration as the GUI edits it. Secrets are never included.

```json
{
  "radio": {"type": "kiss", "port": "/dev/serial/by-id/usb-…", "region": "EU_868", "preset": "LONG_FAST",
            "primary_channel": "", "tx_power_dbm": 27, "frequency_offset_mhz": 0, "baud": 115200,
            "hop_limit": 3, "channel_num": 0, "override_frequency_mhz": 0},
  "relay": {"role": "client", "long_name": "RepeaterTastic Relay", "short_name": "RPTR", "local_dm": "software"},
  "airtime": {"duty_cycle_percent": 10, "identity_share_percent": 25, "nodeinfo_interval": "3h",
              "telemetry_interval": "off", "override_duty_cycle": false, "cw_min": 3, "cw_max": 8},
  "web": {"bind": "0.0.0.0", "port": 8080, "session_ttl": "168h", "map_tile_url": "",
          "map_key_source": "built in", "mdns": false, "log_level": "info"},
  "position": {"latitude": 56.2055, "longitude": -3.1618, "altitude": 90, "precision_bits": 32,
               "interval": "3h", "identities": "relay"},
  "hardware": {"hw_model": "AUTO", "effective": "HELTEC_V3", "modem": "Heltec V3"},
  "mqtt": [MQTTConnection],
  "radio_id": "main", "main": true
}
```

`PUT /config[?radio=<id>]` takes any subset of the sections `radio`, `relay`, `airtime`, `web`,
`position`, `hardware` and `mqtt`, and replies `{"config", "restart_required"}`. The GUI sends one
section at a time. A section replaces the fields it contains; left-out fields keep their values.

| Field | Rules |
| --- | --- |
| `radio.hop_limit` | 1-7 |
| `radio.channel_num`, `radio.override_frequency_mhz` | 0 = derived from the name, region and preset |
| `relay.role` | as [`PUT /relay`](#relay-role) |
| `relay.local_dm` | `software` (DMs between local identities stay on the host) or `also_rf` |
| `airtime.duty_cycle_percent` | the region's value is saved as 0, so it follows the region |
| `airtime.nodeinfo_interval` | a duration of at least `10m` |
| `airtime.telemetry_interval` | `off`, or a duration of at least `30m` |
| `position.interval` | a duration of at least `30m` |
| `position.identities` | `relay` or `all` |
| `web.session_ttl` | a duration of at least `1m` |
| `web.log_level` | `debug`, `info`, `warn` or `error`; applies live |
| `web` | main radio only (400 for another radio) |
| `hardware.hw_model` | `AUTO` or a Meshtastic hardware model name |
| `airtime.cw_min`, `cw_max`, `web.map_key_source`, `hardware.effective`, `hardware.modem`, `radio_id`, `main` | read-only |

An unknown section, or a value that doesn't validate, answers 400. With `?radio=<id>` the change is
written to that radio's `radios:` entry.

**MQTT connections.** `mqtt` is a list, and a PUT replaces the whole list (a single object is
accepted as a one-item list). See [MQTT](mqtt.md) for what the fields mean.

```json
{"key": "public", "name": "public", "enabled": true, "address": "mqtt.meshtastic.org:1883",
 "username": "meshdev", "password": "", "password_set": true, "clear_password": false, "tls": false,
 "root": "msh/EU_868/Scotland", "mode": "gateway", "gateway": "relay", "format": "encrypted",
 "uplink_channels": [], "downlink_channels": [], "channel_selection": "identity",
 "ignore_consent": false, "ok_to_mqtt": true, "relay_mqtt": false, "relay_hops": 0, "cross_link": false,
 "bridge_acknowledged": false, "downlink_per_minute": 30, "uplink_per_minute": 120,
 "map_report": {"enabled": true, "interval": "1h", "position_precision": 14, "latitude": 0, "longitude": 0}}
```

- `name` is required.
- `password` is write-only: empty keeps the saved one, `clear_password` removes it, and
  `password_set` says one is saved.
- `key` is the saved name, so a renamed connection keeps its password.
- `mode: "bridge"` needs `bridge_acknowledged: true`.
- `map_report.interval` is at least `15m`.

### What needs a restart

Most changes apply live. These are saved at once but wait for a restart, and show in
`status.restart_reasons`:

- the modem connection (`radio.type`, `radio.port`, baud), once the modem has opened. Until then a
  new `port` is used at once;
- adding or removing a radio;
- MQTT connections and UDP multicast;
- `web.bind`, `web.port` and `web.mdns`;
- turning a site airtime cap on for a single radio;
- the `plugins:` config section, except the send limits;
- a restored backup.

## Links

`GET /links[?radio=<id>]` → the UDP multicast link, then one entry per MQTT connection.

```json
[{"name": "udp", "type": "udp_multicast", "enabled": false, "group": "", "connected": false,
  "rx": 0, "tx": 0, "detail": "239.0.0.69:4403 + 224.0.0.69:4403"},
 {"name": "mqtt:public", "connection": "public", "type": "mqtt", "enabled": true, "connected": true,
  "broker": "mqtt.meshtastic.org:1883", "tls": false, "root": "msh/EU_868/Scotland", "mode": "gateway",
  "format": "encrypted", "gateway": "relay", "gateway_id": "!3f0a91c2", "rx": 10, "tx": 4, "dropped": 0,
  "uplink": ["LongFast"], "downlink": [], "ok_to_mqtt": true, "relay_mqtt": false, "cross_link": false,
  "map_report": true, "detail": "mqtt.meshtastic.org:1883 · msh/EU_868/Scotland"}]
```

`PATCH /links/udp[?radio=<id>]` `{"enabled", "group"}` → the UDP link plus `restart_required`.
`group` is a multicast `address:port`, or `""` for the default. Other link names answer 404; MQTT
connections are edited through `PUT /config`.

## API tokens, logs, backup and restore

| Method and path | Body → response |
| --- | --- |
| `GET /tokens` | → `[{"id", "name", "created_at", "last_used"}]` (`last_used` is `null` or a time, updated at most hourly) |
| `POST /tokens` | `{"name"}` → 201 `{"id", "name", "token", "created_at", "last_used": null}` |
| `DELETE /tokens/{id}` | → 204, or 404 |
| `GET /logs?limit=` | → `[{"time", "level", "msg", "radio", "identity"}]`, `limit` 1-2000 (default 500); `radio` and `identity` (a node ID) say what the line is about, when it's about one |
| `GET /backup` | → the backup file |
| `POST /restore` | the backup file → `{"restart_required": true}` |

- `token` is shown only once. A token needs a name (400).
- **Backup** is sent as `repeatertastic-backup-YYYY-MM-DD.json`:
  `{"format": "repeatertastic-backup-1", "created", "version", "config_yaml", "identities", "radio_identities"}`.
  It holds the config (with MQTT passwords) and every radio's identities with their private keys.
  `radio_identities` maps extra radio ids to their identities. Store it like a password.
- **Restore** takes the file as downloaded (up to 32 MB). Nothing changes under the running daemon:
  the files are staged and replace the configuration and identities when the daemon next starts.
  400 when it isn't a backup, or its configuration or identities don't validate.

## meshtasticd nodes

The relay persona of each modem or HAT radio, and every identity, runs on meshtasticd
([Real Meshtastic nodes](meshtasticd-nodes.md)). A board radio (`driver: meshtastic`) keeps the
board itself as its relay; its identities still run on meshtasticd, one hop behind it.

| Method and path | Body → response |
| --- | --- |
| `GET /hosted` | → `{"meshtasticd", "docker_image", "port_base", "min_version", "instances", "restart_required"}` |
| `PUT /hosted` | `{"meshtasticd", "docker_image", "port_base"}` → the same as `GET` (applies at restart) |
| `GET /hosted/{name}/log` | → `[{"time", "text"}]`, the last 1000 lines that instance's meshtasticd printed, oldest first |

- `instances` are the meshtasticd processes running now: `{"radio", "role", "name", "launcher",
  "port", "running", "connected", "since", "restarts", "reboots", "last_error", "stops",
  "firmware", "node_id", "sensors"}`. `role` is `persona` or `identity`; `sensors` are the ids of the
  host sensors that node publishes as its own ([Sensors](sensors.md)); `since` is when the current process
  started. `restarts` counts unexpected stops and `reboots` the stops that applied settings
  RepeaterTastic had just given it (meshtasticd reboots for some). `stops` are the last 20, each
  `{"time", "reason", "reboot"}`. `GET /hosted/{name}/log` has the instance's output.
- `GET /setup/runtimes` (no token during setup) → `{"meshtasticd": {"found", "path", "version",
  "ok", "error"}, "docker": {"found", "ok", "version", "error", "image", "image_present"},
  "min_version"}`: whether meshtasticd is installed and new enough, and whether Docker answers and
  already has the image. It never downloads anything.
- `PUT /hosted` always checks meshtasticd first and answers 400 when it can't run or is older than
  `min_version` — there's no fallback, so a node whose meshtasticd can't run just stays off air.
  `meshtasticd` must name a meshtasticd program (`""` = the one on `PATH`); `docker_image`, when
  set, runs it in Docker instead.
- `POST /setup` takes the same object as `hosted`; before a password exists only
  `meshtastic/meshtasticd` images are accepted. `POST /setup/meshtasticd` checks a program or image
  the same way and always answers 200 with `ok` and `error`, except for a program that isn't
  meshtasticd (400) or, before a password exists, an image that isn't `meshtastic/meshtasticd` (400).

## Sensors

Host readings published by identities as their own sensors ([Sensors](sensors.md)). Sensors belong to
the host, not to a radio, so these endpoints ignore `?radio=`.

A Sensor:

```json
{"id": "shed", "name": "Shed", "kind": "exec", "command": "/usr/local/bin/read-shed",
 "interval": "5m0s", "scale": {"current": 0.001},
 "last": {"at": "2026-09-24T12:00:03Z", "fields": {"temperature": 18.4, "humidity": 63.2}},
 "age_ms": 12000, "fresh": true, "reads": 412, "errors": 0, "error": "",
 "identities": ["!a1c40e07", "!00ff1234"]}
```

`kind` is `exec`, `file` or `push`. `fresh` is false once a reading is older than three times the
sensor's interval (15 minutes for a push sensor), which is how the GUI greys a stale value. `error`
is the last read's failure, kept while the previous good reading stays in `last`. `identities` are
the node ids publishing it.

| Method and path | Body → response |
| --- | --- |
| `GET /sensors` | → `{"sensors": [Sensor], "interval": "1h0m0s", "fields": [{"field", "unit", "chip"}]}` |
| `POST /sensors` | `{"id", "name", "kind", "command", "path", "interval", "scale"}` → the Sensor (409 if the id is taken) |
| `PUT /sensors/{id}` | the same → the Sensor |
| `DELETE /sensors/{id}` | → `{"ok": true}`, and detaches it from every identity |
| `POST /sensors/{id}/read` | → the Sensor after reading it now, or 400 with what the command printed |
| `POST /sensors/{id}/push` | `{"temperature": 18.4}` → the Sensor (400 unless `kind` is `push`) |
| `GET /sensors/{id}/history?since=1h` | → `[{"at", "fields"}]`, oldest first, up to 720 readings |
| `PUT /sensors/interval` | `{"interval": "1h"}` → `{"interval": "1h0m0s"}`, how often nodes broadcast (min 30m) |
| `GET /identities/{id}/sensors` | → `[{"sensor", "fields"}]` for one identity |
| `PUT /identities/{id}/sensors` | `[{"sensor", "fields"}]` → the same, and restarts that identity's node |

- `interval` fields are Go durations (`"5m"`, `"30s"`); a sensor is read no more often than every 5 s.
- `fields` in `GET /sensors` is the catalogue the GUI offers: every field, its unit, and the chip the
  node will think it has. A field no chip can carry is refused with 400.
- `PUT /identities/{id}/sensors` bounces that identity's meshtasticd, because it only scans for
  sensors at start-up. Nothing else on the host is interrupted. Changing a sensor's command, path,
  interval or value applies live.
- `POST /sensors/{id}/push` accepts a flat object of field names to numbers. Unknown fields are
  ignored; a body with no usable field answers 400. Plugins push with the `sensors` permission
  instead of a token.
- Every change here is written to `repeatertastic.yaml`, the same file the [`sensors:`
  section](sensors.md#setting-it-up-in-the-config-file) uses.

## Plugins

See [Plugins](plugins.md) for the operator guide and [Plugin API](plugin-api.md) for what plugins
themselves speak. With `plugins.enabled: false`, `GET /plugins` returns
`{"enabled": false, "plugins": []}` and every other plugin endpoint answers 503.

A Plugin:

```json
{"id": "hello", "name": "Hello Mesh", "version": "1.0.0", "description": "…", "author": "…",
 "homepage": "…", "license": "…", "kind": "managed", "enabled": true, "pinned": false,
 "state": "running", "detail": "",
 "permissions": [{"key": "packets.read", "text": "See every packet…", "granted": true}],
 "network": ["api.example.org"], "settings": [Setting], "values": {"api_key": "••••••••"},
 "secrets_set": ["api_key"], "status": {"summary": "Uploading", "state": "ok", "fields": {"Packets": "1204"}},
 "has_logo": true, "has_panel": true, "logo_url": "/plugin-assets/hello/<key>/logo",
 "panel_url": "/plugin-assets/hello/<key>/panel/", "connected": true, "connected_at": 1757900000000,
 "started_at": 1757899990000, "restarts": 0, "dropped_events": 0, "installed_at": 1757800000000,
 "source": "upload"}
```

- `kind` is `managed` (RepeaterTastic runs it) or `attached` (it connects from elsewhere).
- `pinned` means `plugins.entries` in the config file sets it.
- `state` is one of `disabled`, `needs_review`, `needs_settings`, `starting`, `running`,
  `restarting`, `crashed`, `stopped`, `waiting` or `unsupported`. `detail` explains it.
- `settings` is the manifest's settings schema ([reference](plugin-api.md#settings)). `values` holds
  the saved values, with secrets shown as `"••••••••"`; `secrets_set` lists the secrets that are saved.
- `source` is `upload`, `url`, `inbox`, `folder`, `cli` or `attach`.
- Empty optional fields are left out.

| Method and path | Body → response |
| --- | --- |
| `GET /plugins` | → `{"enabled", "error", "plugins": [Plugin], "permissions": {key: text}, "identities", "attach_address", "allow_url_install", "folder", "messages_per_hour", "traceroutes_per_hour", "store": {"enabled", "updates"}}` |
| `POST /plugins` | multipart field `bundle`, or JSON `{"url"}` → 201 Plugin |
| `GET /plugins/store[?refresh=1]` | → `{"enabled", "url", "fetched_at", "error", "plugins": [StorePlugin]}` |
| `POST /plugins/store/{id}/install` | → 201 Plugin |
| `PUT /plugins/limits` | `{"messages_per_hour", "traceroutes_per_hour"}` → the same |
| `POST /plugins/attach` | `{"id", "name", "permissions": []}` → 201 `{"plugin", "token", "address"}` |
| `GET /plugins/{id}` | → Plugin |
| `DELETE /plugins/{id}[?keep_data=1]` | → 204 |
| `POST /plugins/{id}/enable` | `{"permissions": []}` → Plugin |
| `POST /plugins/{id}/disable` | → Plugin |
| `POST /plugins/{id}/restart` | → Plugin |
| `PUT /plugins/{id}/settings` | `{key: value}` → Plugin |
| `POST /plugins/{id}/token` | → `{"token", "address"}` |
| `GET /plugins/{id}/logs` | → `[{"time", "level", "source", "message"}]` |
| `GET /plugins/{id}/panel-data` | → the plugin's panel JSON, or `null` |
| `POST /plugins/{id}/panel-action` | `{"name", "payload"}` → 202 |

A StorePlugin, from `GET /plugins/store`:

```json
{"id": "meshflow", "name": "Meshflow", "summary": "Reports what each radio hears…",
 "description": "…", "author": "ScotMesh", "homepage": "…", "license": "GPL-3.0-or-later",
 "tags": ["mapping"], "permissions": ["packets.read"], "network": ["your Meshflow API server"],
 "latest": {"version": "0.1.1", "api": 1, "min_host": "0.3.0", "released": "2026-09-15T19:00:58Z",
            "url": "https://…/plugin-v0.1.1.zip", "sha256": "90f0fe…", "size": 14498857,
            "arches": ["linux/amd64", "linux/arm64"], "notes": "First public release."},
 "logo_url": "/plugin-store-logo/<key>/meshflow", "installed": "0.1.0", "update_available": true}
```

- `installed` is the version running here, left out when it isn't installed; `update_available`
  compares it with `latest.version` numerically, so 0.10.0 beats 0.9.0.
- `unusable` says why this node can't install it (no build for this CPU, a newer RepeaterTastic
  needed, a permission this version doesn't have, or it runs in its own container). It is left out
  when the plugin can be installed.
- `image` marks a plugin that runs in its own container: attach it rather than installing it.
- The logo is served by the daemon, not the store, so a browser that can't reach the store still
  shows it. The key lasts until the daemon restarts.

- **Store:** `GET /plugins/store` answers 200 with `{"enabled": false}` when `plugins.store_url`
  is `off`. A store that can't be read still answers 200, with the cached list and an `error`
  saying why it may be out of date. `?refresh=1` asks the store instead of using the cached copy.
- **Install from the store:** the daemon downloads the bundle, checks it against the `sha256` in
  the index and checks the bundle calls itself the id on the card. Either mismatch is a 400 and
  nothing is installed. The plugin arrives switched off, like any other install.

- **List:** `error` says why the plugin system couldn't start (`""` when it did); the daemon keeps
  running without plugins. `identities` lists every identity on every radio as
  `{"node_id", "long_name", "short_name", "radio_id", "radio_name", "is_relay"}`: the choices for
  `identities` settings. `attach_address` is the TCP address attached plugins connect to (`""` when
  `plugins.listen` is off). `folder` is the inbox folder.
- **Install:** a new plugin is installed switched off; a bundle with an installed id upgrades it.
  Bundles are up to 100 MB (413 above that). A URL install answers 403 unless
  `plugins.allow_url_install` is on. 400 when the bundle is refused, with the reason.
- **Limits** are per plugin: 0-600 messages and 0-120 traceroutes an hour (400 outside that; 0 stops
  plugins sending). They apply to every plugin at once, are saved to the config, and need no restart.
- **Attach** registers a plugin that runs elsewhere, switched on with the given permissions. The
  token is shown once. 400 for a bad id (2-40 lowercase letters, digits and dashes) or an unknown
  permission; 409 when the id is taken.
- **Enable** grants `permissions`, which must be ones the plugin asks for. It answers 400 while
  required settings are empty. Changing a running plugin's grants reconnects it.
- **Restart** stops and starts the plugin, or retries a crashed one. 409 while it can't run
  (disabled, needs settings or a permission review).
- **Settings:** `null` or `""` clears a value, and sending the secret mask keeps a saved secret.
  Values are checked against the schema (400). 409 before an attached plugin has sent its manifest.
  A running plugin gets the new values at once.
- **Token** replaces an attached plugin's token and disconnects it. 404 for managed plugins.
- **Logs** are the last 1000 lines. `source` is `plugin`, `stdout`, `stderr` or `host`.
- **Panel action** passes `{"name", "payload"}` from the plugin's panel to the plugin. `name` is
  1-100 characters. 409 when the plugin isn't connected.
- **Delete** stops and removes the plugin, and its data folder unless `keep_data=1`.
- A plugin pinned by `plugins.entries` answers 409 to enable, disable, settings and delete. An unknown
  id answers 404.

### Plugin assets

`GET /plugin-assets/{id}/{key}/logo` and `GET /plugin-assets/{id}/{key}/panel/<file>` (`panel/` alone
serves the panel's HTML file). These are outside `/api/v1` and need no bearer token, because `<img>`
and `<iframe>` can't send one; `logo_url` and `panel_url` carry the key instead. Keys last until the
daemon restarts or the plugin is removed. A wrong key or missing file answers 404. Files are served
with a sandbox Content-Security-Policy: scripts run in an opaque origin with `connect-src 'none'`.
