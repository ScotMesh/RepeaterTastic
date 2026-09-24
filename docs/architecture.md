# Architecture and development

[← README](../README.md) · [Hardware](hardware.md) · [Configuration](configuration.md) · [Web GUI](web-gui.md) · [Several radios](radios.md) · [MQTT](mqtt.md) · [Plugins](plugins.md) · [HTTP API](api.md)

## How it fits together

RepeaterTastic doesn't run Meshtastic nodes itself. The **host** is an air bridge: it owns the
radio, the transmit queue and duty cycle, the packet log and node database, browser chat, MQTT/UDP
links and plugins, and it serves the Meshtastic client API on each identity's app port. The
**nodes** — the relay persona and every identity — are real Meshtastic firmware: a `meshtasticd`
RepeaterTastic starts, seeds with the identity's key, and joins to the host's air (`internal/nodes`,
[meshtasticd nodes](meshtasticd-nodes.md)). meshtasticd does the encrypting, transmitting,
relaying, ACKing and NodeInfo/position/telemetry broadcasting; the host never does.

```
LoRa modem ──USB/KISS── radio driver ── air bridge ── mesh host ──┬── meshtasticd "Base Camp" ── :4403 (apps, CLI, web client)
                                        (dedupe, log,   (queue,    ├── meshtasticd "Ops Desk"  ── :4404
                                         node DB)        duty      ├── meshtasticd relay persona
                                                          cycle)   └── UDP multicast link (meshtasticd LAN mesh)
                                                  web GUI + REST/SSE API :8080
```

- **Receive:** the host decodes each frame only to log it and keep the node database and links
  current — the node that hears it, over the air bridge, does the actual decrypting, delivering,
  ACKing and relaying.
- **Transmit:** every joined node's packets go through one queue, which uses Meshtastic's contention
  window, a channel-busy check before each transmission, and the region duty cycle.
- **Relay:** only the relay persona rebroadcasts, following its firmware's flooding and next-hop
  rules. N identities never relay the same packet N times.
  In `monitor` mode the radio transmits nothing, and in `off` mode it is ignored; sends then fail
  with `NO_INTERFACE`.
- **A board radio** (`driver: meshtastic`) keeps the board itself as the relay; its identities still
  run on meshtasticd, a hop behind it, reached over the board's MQTT client proxy.
- **Several radios:** each radio is its own mesh host (air bridge, node DB, queue) with its own
  `nodes.Hosting`. `internal/site` makes radios on overlapping frequencies take turns and applies
  the site airtime cap; `mesh.JoinSite` lets radios on one mast see each other's identities and node
  sightings (`GET /nodes/{id}/sightings`).
- **Links:** UDP multicast and MQTT connections see packets as they're received and sent, and inject
  broker packets into the receive pipeline marked `via_mqtt`.
- **App API:** each identity listens on its own TCP port with the Meshtastic client protocol
  (`internal/phoneapi`); packets addressed to the identity itself, admin included, are forwarded to
  its meshtasticd. The web GUI and scripts use the REST/SSE API on `:8080`.
- **Plugins:** separate programs, started by `internal/plugins` or attached over TCP, talk gRPC to
  the Plugin API host. They see bus events their permissions allow, and send through the same queue
  within a per-plugin budget.
- **Sensors:** `internal/sensors` samples a host reading once and writes it into each attached
  identity's node directory; a shim inside that node's meshtasticd answers its I²C reads from the
  file, so the node detects, reads and broadcasts the sensor as its own ([`sensors.md`](sensors.md)).

The original plan and research are in [`plan.html`](plan.html). The HTTP API is documented in
[`api.md`](api.md), and the Plugin API in [`plugin-api.md`](plugin-api.md). Coding agents and new contributors: read [`AGENTS.md`](../AGENTS.md) first.

## Layout

```
cmd/repeatertastic     daemon
cmd/kisstool           modem bench tool: info, listen, send-text
internal/wire          16-byte header, AES-CTR channels, X25519 + AES-CCM DMs, AEAD channels
internal/phy           regions, presets, frequency slots, airtime, contention window
internal/mesh          host: identities, node DB, transmit queue and duty cycle, receive (dedupe,
                        log, feed the links), site joins for sightings across a mast (site.go)
internal/nodes         meshtasticd: launches, seeds and bridges the relay persona and every
                        identity, health for the status bar (see meshtasticd-nodes.md)
internal/phoneapi      Meshtastic client API: TCP stream + HTTP, config handshake, local admin
internal/mtclient      Meshtastic client API, client side: drives a board or meshtasticd (see meshtasticd-nodes.md)
internal/radio         radio interface; kiss (serial), sim (tests), null
internal/links/udp     meshtasticd UDP multicast link
internal/links/mqtt    Meshtastic MQTT connections (gateway, monitor, bridge, map reports)
internal/site          several radios on one host: shared transmit turns and airtime cap
internal/config        YAML config, validation, environment overrides
internal/web           REST/SSE API, auth, embedded GUI
internal/plugins       plugin bundles, supervisor, Plugin API host, permissions and budgets
internal/sensors       host sensor sources, the values file each hosted node reads (see sensors.md)
api/plugin/v1          Plugin API: plugin.proto and its generated Go (scripts/gen-plugin-proto.sh)
sdk/                   Go client for plugin authors
examples/plugins/hello example plugin with a panel (make plugin-example)
api/meshtastic         vendored Meshtastic .proto files and their generated Go, package pb (scripts/gen-proto.sh); public for plugins
ui/                    web GUI (Vue 3 + Vite), built into internal/web/dist
firmware/              KISS modem patch, board list, build script
shim/                  the I²C shim a hosted node loads so it owns its sensors (see sensors.md)
tests/interop          meshtasticd Docker harness + golden vectors
deploy/                systemd unit, example config, install script (sets up meshtasticd too), docker-compose example
Dockerfile             container image: the official meshtasticd image, with repeatertastic added
```

## Development

```bash
make test race          # unit tests, simulated multi-host mesh tests, golden vectors
make lint               # golangci-lint and vue-tsc
make ui                 # rebuild the GUI into internal/web/dist (commit the result)
cd ui && npm run dev    # GUI against a mock API
RT_TEST_MQTT_BROKER=127.0.0.1:1883 go test ./internal/links/mqtt   # MQTT against a real broker
cd tests/interop && ./run_meshtasticd.sh up 2 && ./run_repeatertastic.sh up   # real firmware interop
```

SonarQube (`sonar-project.properties`) is for reading, on demand: cognitive complexity, duplication
and coverage per package. Run `go test ./... -coverprofile=coverage.out`, then the scanner as the
file describes.

### Checks before a pull request

- `go vet ./... && go test ./...` (add `-race` for `internal/mesh`, `internal/site`, `internal/links`)
- `cd ui && npm run build` when anything under `ui/` changed, committing `internal/web/dist`
- README, the guides in `docs/` and `deploy/repeatertastic.example.yaml` updated with the change
