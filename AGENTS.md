# AGENTS.md

Guidance for coding agents (and humans) working on RepeaterTastic. Read this before changing code.

## What this is

A Go daemon that bridges one or more Mesh KISS LoRa modems (or a LoRa HAT/USB stick, or a board on
Meshtastic firmware) to many Meshtastic nodes ("identities"), each a real `meshtasticd` instance the
daemon starts, seeds with its key and joins to the radio. It doesn't run nodes itself: the host is
the air bridge, transmit queue, packet log, node database, links and client API; meshtasticd does
the encrypting, transmitting, relaying and ACKing. A Vue 3 web GUI is embedded in the binary. See
`README.md` for the feature tour, `docs/architecture.md` for how the host and meshtasticd split the
work, and `docs/api.md` for the HTTP API.

## Commands

```bash
go vet ./... && go test ./...        # must pass before every commit
go test -race ./...                  # for changes to internal/mesh, internal/site, internal/links
make lint                            # golangci-lint (.golangci.yml) and vue-tsc; must report 0 issues
cd ui && npm ci && npm run build     # vue-tsc + vite; writes internal/web/dist (commit it)
make shim && make shim-test          # the hosted-node I²C shim (internal/nodes/shim is committed)
make build | make dist               # binaries; MAP_API_KEY=... bakes in the map tile key
docker build -t repeatertastic .     # container image
RT_TEST_MQTT_BROKER=host:1883 go test ./internal/links/mqtt   # MQTT against a real broker
```

`internal/web/dist` is committed: any change under `ui/` needs `npm run build` and the rebuilt
`dist` in the same commit, or the daemon serves the old GUI. `internal/nodes/shim/*.so` is committed the
same way and for the same reason — the daemon embeds it — so a change to `shim/*.c` needs
`make shim` (which builds amd64, arm64 and armv7 and copies them there) and the rebuilt libraries in the
same commit. `make shim-test` replays the firmware's own I²C call sequences against them.

## Layout

- `cmd/repeatertastic` daemon entry point (flags, env overrides, radio start-up, starting each
  radio's meshtasticd nodes).
- `internal/sensors` host sensor sources and the values file each hosted node reads; `shim/` is the
  I²C shim that makes a stock meshtasticd believe those sensors are its own (`docs/sensors.md`).
  Sensors are configured in one place: the `sensors:` section of the config file, which the GUI edits
  and saves. Never add a second store for them.
- `internal/mesh` the host: identities, node DB, transmit queue and duty cycle, receive (dedupe,
  log, feed the links), site joins for sightings across a mast (`site.go`). It no longer transmits,
  relays, ACKs or broadcasts NodeInfo/position/telemetry itself — meshtasticd does.
- `internal/nodes` meshtasticd: launches, seeds and bridges the relay persona and every identity
  (`hosting.go`, `hosted.go`, `board.go`), health for the status bar (`hosting.go`'s `Health`).
- `internal/phoneapi` the Meshtastic client API each identity serves (TCP stream + HTTP); packets
  addressed to the identity itself, admin included, are forwarded to its meshtasticd.
- `internal/web` REST/SSE API and auth; `server.go` registers routes (`pub`, `setup`, `priv`), and
  each area has its own file (`setup.go`, `identities.go`, `channels.go`, `messages.go`, `nodes.go`,
  `stats.go`, `config.go`, `links.go`, `radios.go`, `meshtasticd.go`, `plugins.go`, `backup.go`).
- `internal/config` YAML config, defaults, validation, `ApplyEnv`.
- `internal/links/mqtt`, `internal/links/udp`, `internal/site` (several radios on one host).
- `ui/src` GUI: `views/` pages, `components/` (identities, config, packets, nodes, layout, ui),
  `store/live.ts` shared live state, `api/types.ts` API types (keep in step with the Go JSON).

## Rules that matter

- **Airtime is shared with real people.** Never add traffic that repeats on a timer without a
  duty-cycle check (`dutyLimit`, channel utilisation) and a sensible minimum interval. Only the
  relay persona repeats; new identities default to `CLIENT_MUTE`.
- **One way to do each thing.** The GUI has one dialog per job (for example
  `ChannelSlotDialog.vue` for every channel slot add/edit). Extend it; don't add a second path.
- **Controls must save.** Every GUI control maps to a config or API field that round-trips; test it
  in `internal/web/*_test.go`.
- **No node logic in the host.** `internal/mesh` bridges the radio and keeps the queue, packet log,
  node DB, links and client API; only meshtasticd (`internal/nodes`) encrypts, transmits, relays or
  answers on a node's behalf, and there's no fallback — an identity whose meshtasticd can't run
  stays off air rather than falling back to Go code. Don't add packet-crafting logic back into
  `internal/mesh`.
- **Secrets:** never log or print MQTT passwords, private keys, API tokens or the map API key.
  The map key reaches builds only via `-X main.mapAPIKey` / a BuildKit secret, and at run time via
  `REPEATERTASTIC_MAP_API_KEY`. JSON uses `json:"-"` for passwords; keep it that way.
- **Config files** are written by yaml.v3 with 4-space indentation; don't hand-edit them with
  2-space inserts. Validation lives in `config.Validate`; add new fields there and to
  `deploy/repeatertastic.example.yaml`.
- **Locks:** don't call into another host (radio) while holding a host's `mu`/`chanMu`; `site.go`
  reads identities and node DBs across a mast's hosts.
- **Protobufs** live in `api/`, each `.proto` next to the Go generated from it: `api/meshtastic` (Meshtastic, package `pb`) and `api/plugin/v1` (plugin API); the Go is generated (`scripts/gen-proto.sh`, `scripts/gen-plugin-proto.sh`); don't edit by hand. Plugin work is described in `docs/plugins.md`.

## Style

- Go: small functions, comments that say why, errors that tell the user what to do
  ("slot 0 is the default radio's primary channel; change the identity's default radio instead").
- GUI copy: plain words from the user's side ("Restart to apply", not "restart_required"),
  sentence case, no jargon without an explanation.
- Tests: table-driven where it helps; simulated radios (`internal/radio/sim`) for mesh behaviour.
- Commits: imperative subject, a body that explains the change and why.

## Before you finish

1. `go vet ./... && go test ./...` pass, `make lint` reports 0 issues; `npm run build` passes if `ui/` changed, and `dist` is committed.
2. README, `docs/api.md` and the example config match the change.
3. No secrets in code, logs, tests or commit messages.
