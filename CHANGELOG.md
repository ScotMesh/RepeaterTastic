# Changelog

What changed in each release, in the order they happened. Dates are when the tag was cut.

The version is whatever `git describe` says, so a build between releases calls itself
`v0.5.0-3-gabc1234`: three commits after v0.5.0.

## v0.5.0 — 2026-09-24

**Sensors.** A sensor on the machine running RepeaterTastic can now be published by any of your
identities — as many as you like — each broadcasting it as **its own** sensor.

RepeaterTastic still sends no telemetry packet of its own. It samples the reading and writes it into
each node's directory; a small `LD_PRELOAD` shim (`shim/`) answers that node's I²C traffic from the
file, so a **stock, unpatched meshtasticd** detects the "sensor", reads it on its own schedule,
broadcasts it and answers requests itself. No root, no kernel module, no forked firmware, and no I²C
hardware — the bus it opens need not exist.

- **Readings that reach the air:** temperature, humidity, pressure, lux, `pm10`/`pm25`/`pm100`,
  voltage, current, distance, and rainfall over 1 and 24 hours. Each was watched arriving at an app
  connected to a real meshtasticd, not merely implemented.
- **Where a reading comes from:** a command to run, a file to read, or a push from the HTTP API or a
  plugin. **Test** in the Add dialog runs it before anything is saved.
- **Set up either way:** one `sensors:` section in `repeatertastic.yaml`, which the GUI edits and
  saves — there is no second store. A new **Sensors** page, an attach dialog that says which nodes
  will restart before you confirm, and a Sensors tab in the identity drawer.
- **Plugins** can feed a `push` sensor with the new `sensors.publish` permission (`ListSensors`,
  `PublishSensor`, and an SDK helper). They cannot create a sensor or choose who publishes it.
- **Refusals that explain themselves:** a field no imitated chip carries, and attaching `pressure`
  or `humidity` without a `temperature` — a barometer measures temperature to compensate its own
  reading and the node would otherwise broadcast one nobody measured.
- `radiation` is implemented but not offered: Portduino returns received bytes through a signed
  `char` buffer, so a RadSens reading of 13.7 µR/h reaches the mesh as 429496736. `shim/README.md`
  has the detail, and the self-test will fail the day it is fixed upstream.
- The shim is built for amd64, arm64 and armv7 (`make shim`) and carried inside the binary. Its
  self-test replays the firmware's own I²C call sequences — 104 assertions in about a second, no
  containers.

Docs: [`docs/sensors.md`](docs/sensors.md), plus the sensors sections of the API, GUI, configuration,
architecture and plugin guides.

## v0.4.0 — 2026-09-18

A plugin store: browse, install and update plugins from the GUI, with each download checked against
the sha256 the store lists.

## v0.3.3 — 2026-09-18

Airtime kept per minute and both histories kept across a restart. The dashboard charts airtime and
noise floor over 24 hours, with a settable span and resolution. Hosted nodes are no longer handed
frames too big for meshtasticd's simulated radio. The repo root was tidied: protobufs under `api/`,
the plugin SDK under `sdk/`.

## v0.3.2 — 2026-09-17

Saving the same file at the same moment no longer fails.

## v0.3.1 — 2026-09-17

Sidebar tidying: the sign-out button goes (the account menu in the top bar has it), and "Made in
Scotland" moves to the bottom.

## v0.3.0 — 2026-09-16

Every identity runs on a real meshtasticd, including the relay persona: the daemon launches, seeds
and bridges them. Apps may be allowed to change a node's settings. Plugins can run in Docker, and
the container image carries the binaries on the PATH.

## v0.2.1 — 2026-09-15

Several MQTT connections at once, and the phases of work leading to it.

## v0.1.0-bench — 2026-09-15

First release for bench testing: Pi binaries and the KISS modem firmware for 85 boards, with the
checklist in `docs/bench-test.md`.
