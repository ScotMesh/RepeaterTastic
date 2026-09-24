![ScotMesh Meshtastic](https://raw.githubusercontent.com/ScotMesh/branding/main/networks/meshtastic/readme-header.png)

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/images/repeatertastic-lockup-dark.png">
    <img src="docs/images/repeatertastic-lockup-light.png" width="430" alt="RepeaterTastic: virtual Meshtastic nodes">
  </picture>
</p>

# RepeaterTastic

**Many Meshtastic nodes, one LoRa modem.** RepeaterTastic runs as many Meshtastic nodes as you
like on a Raspberry Pi (or any Linux box) with a cheap LoRa board on USB. Every node is a real
Meshtastic node, a meshtasticd RepeaterTastic starts and shares the radio between. One of them repeats; the rest are yours
to chat, bridge and experiment with. Built and run by [ScotMesh](https://github.com/ScotMesh) for
Scottish mesh sites, and useful anywhere.

![The RepeaterTastic dashboard: radio status and relay switch, traffic counters, airtime against the duty cycle, noise floor, packets by port and live packets](docs/images/dashboard.png)

- **Nodes ("identities")** with their own key, node number, channels and app port, each running on
  its own meshtasticd. The official Meshtastic apps, the Python CLI and the web client connect to
  each one as if it were a radio.
- **A proper repeater:** one relay persona, itself a meshtasticd, does the repeating with a shared
  duty-cycle budget, so ten identities never mean ten repeats. Its role is a Meshtastic device
  role (client, client base, client mute, router, router late), or put the radio in Monitor (listen only) or Off from the top bar.
- **A web GUI** for everything: identities, chat (as any identity, the relay persona included),
  channels, a live node map, packets, statistics, logs, backups and configuration.
- **Several radios on one host** (LongFast, MediumFast, …) that take turns on shared frequencies.
- **MQTT** to one or more brokers (gateway, uplink-only, map reports, monitor, bridge), a fixed
  site position, device telemetry and an optional UDP multicast link to `meshtasticd` on the LAN.
- **Sensors** on the host, published by whichever identities you choose — each one broadcasting the
  reading as its own sensor, on stock meshtasticd, with no root and no I²C hardware needed.
- **Plugins** add uploaders, bots and dashboards: upload a .zip in the GUI or drop it in a folder,
  choose what each one may see, and cap how much it may send.
- **One container, or one binary plus meshtasticd.** The image includes meshtasticd; standalone,
  the installer sets it up. The top bar shows whether every node is running.

> **Status:** running on a ScotMesh site on a Heltec V3, alongside Reticulum. Still young: expect
> changes. Needs meshtasticd 2.8.0 or newer. Verified on air with KISS modems, a CH341 stick and a
> board on Meshtastic firmware: channel messages, PKI DMs with ACKs, local DMs between identities,
> MQTT, and the official apps and CLI on the identity ports.

## Quick start

You need a LoRa radio: a board flashed with the Mesh KISS modem firmware, a LoRa HAT or CH341 USB
stick, or a board running Meshtastic firmware. See [Hardware and modems](docs/hardware.md) for which
boards work and how to flash them.

### Run it on a Raspberry Pi

```bash
git clone https://github.com/ScotMesh/RepeaterTastic && cd RepeaterTastic
sudo ./deploy/install.sh
```

This installs meshtasticd and the latest RepeaterTastic release as a service. It works on
Raspberry Pi OS, Debian and Ubuntu.

### Or run it in Docker

```bash
docker run -d --name repeatertastic --restart unless-stopped --init --network host \
  --device /dev/ttyUSB0 --group-add "$(getent group dialout | cut -d: -f3)" \
  -v repeatertastic-data:/data ghcr.io/scotmesh/repeatertastic:latest
```

The image includes meshtasticd. The `/data` volume holds your keys and chats, so back it up. For
Compose, HATs and USB sticks, see [Docker](docs/hardware.md#docker).

### Then open the browser

1. Go to `http://<host>:8080` and choose an admin password. The setup wizard finds the radio and
   meshtasticd.
2. When the **meshtasticd** chip in the top bar turns green, every node is running.
3. **Identities → New identity** creates a node. In the Meshtastic app, add a network device at
   `<host>:<port>`.

### Build it yourself

```bash
make ui                                  # web GUI → internal/web/dist (Node 22)
make build                               # bin/repeatertastic, bin/kisstool (run with meshtasticd 2.8+)
make dist                                # static binaries for Pi (arm64/armv7/armv6) and amd64
docker build -t repeatertastic .         # container image, meshtasticd included
./firmware/build.sh Heltec_v3_kiss_modem # modem firmware (PlatformIO)
```

Go 1.25+ is needed. `MAP_API_KEY=… make build` (or `--secret id=map_api_key,env=CARTO_API_KEY` for
Docker) bakes in a default map tile key; see [Configuration](docs/configuration.md#web-and-map-tiles).

## Sensors

A sensor on the machine running RepeaterTastic can be published by any of your identities — as many
as you like — each broadcasting it as **its own** sensor. RepeaterTastic never sends the telemetry
packet: it writes the reading into each node's folder, and a small shim makes that node's stock
`meshtasticd` believe it owns an I²C chip, so the node reads it and broadcasts it on its own
schedule. No root, no I²C hardware, nothing patched. Full guide: [Sensors](docs/sensors.md).

### Set one up in the GUI

1. **Sensors → Add sensor.** Give it a name and an id, then say where the reading comes from:
   **Run a command**, **Read a file**, or **Pushed to us** (a script or plugin sends it in).
2. Whatever produces the reading prints **one `field=value` per line**. The dialog lists the field
   names a node can publish — `temperature`, `humidity`, `pm10`, `pm25`, `pm100`, `voltage`,
   `current` — and anything else in the output is ignored.
3. **Test** in the dialog runs it before anything is saved and shows exactly what came back, so a
   typo is obvious before the sensor exists at all. **Read now** does the same for a saved one.
4. **Attach**, tick the identities that should publish it, and choose each one's fields. Until you
   do, the card says so in amber — a sensor reads happily on its own, but nothing reaches the air
   until an identity publishes it. Attaching restarts those identities' nodes (a `meshtasticd` looks
   for sensors only when it starts) and the dialog says which ones will bounce before you confirm.
5. **Broadcast every** (top of the page) sets how often each node puts its readings on air: 1 hour by
   default, 30 minutes minimum. That is one packet **per identity** per interval, counted in the
   site's airtime budget.

Everything the GUI does is written to `repeatertastic.yaml`, and that file can be written by hand
instead — the two are the same thing.

### Examples

A Raspberry Pi's own CPU temperature, no hardware needed:

```yaml
sensors:
    sources:
        - id: pi-cpu
          name: Pi CPU
          kind: exec
          command: awk '{printf "temperature=%.1f\n", $1/1000}' /sys/class/thermal/thermal_zone0/temp
          interval: 1m
    attach:
        - {sensor: pi-cpu, identities: [all], fields: [temperature]}
```

A BME280 on the Pi's own I²C bus, read by whatever tool you like, published by two personas (the
snippets below continue the same `sources:` and `attach:` lists):

```yaml
        - id: outside
          name: Outside
          kind: exec
          command: /usr/local/bin/read-bme280      # prints temperature= and humidity=
          interval: 5m
    attach:
        - {sensor: outside, identities: [BASE, "!a1c40e07"], fields: [temperature, humidity]}
```

A battery shunt whose script reports milliamps, corrected on the way in:

```yaml
        - id: battery
          kind: file
          path: /run/battery.values                # voltage=12.6 / current=1450
          interval: 30s
          scale: {current: 0.001}                  # mA → A
```

An air quality sensor fed from elsewhere — Home Assistant, a cron job, anything that can POST:

```yaml
        - id: air
          name: Air quality
          kind: push                               # POST /api/v1/sensors/air/push {"pm25": 7.4}
```

Which readings reach the air is fixed by the chips the shim imitates: temperature, humidity,
`pm10`/`pm25`/`pm100`, voltage and current. Pressure, lux, distance, radiation and rainfall are
refused with a message saying so rather than silently dropped —
[the table in the guide](docs/sensors.md#which-fields-can-be-published) has the details, and adding
one is a chip model in `shim/i2cshim.c`.

## Documentation

| Guide | What's in it |
| --- | --- |
| [Hardware and modems](docs/hardware.md) | Supported boards, flashing Mesh KISS, stable device paths, permissions, Docker devices, `kisstool`, troubleshooting |
| [meshtasticd nodes](docs/meshtasticd-nodes.md) | How the relay persona and identities run on meshtasticd, the status indicator, and measured firmware behaviour |
| [LoRa HATs and USB sticks](docs/spi-radio-testing.md) | Driving a Pi HAT or CH341 stick directly (`radio.driver: spi`): supported boards, setup, and how to test one |
| [Configuration](docs/configuration.md) | The config file section by section, environment variables, what applies live, backups |
| [Using the web GUI](docs/web-gui.md) | Identities, chat, channels, nodes and map, packets, statistics, configuration tabs |
| [Several radios](docs/radios.md) | Running LongFast and MediumFast side by side, moving identities between them, and the site airtime cap |
| [MQTT](docs/mqtt.md) | Broker connections, modes, channels, relaying and map reports |
| [Sensors](docs/sensors.md) | Publishing a host sensor on as many identities as you like: sources, fields, attaching, pushing readings |
| [Plugins](docs/plugins.md) | Installing plugins (GUI, folder, CLI, Docker), permissions, attached plugins, and writing your own |
| [Architecture and development](docs/architecture.md) | How it fits together, code layout, tests and interop |
| [Plugin API](docs/plugin-api.md) | Reference for plugin authors: the gRPC session, calls, events, errors, manifest and settings |
| [HTTP API](docs/api.md) | REST and event-stream API for scripts and integrations |
| [Bench test](docs/bench-test.md) | Step-by-step first test on a real radio |
| [AGENTS.md](AGENTS.md) | Rules and commands for coding agents and contributors |

## Licence

GPL-3.0-or-later. The Meshtastic protobufs are GPL-3.0. Parts of the web GUI are adapted from
openHop Repeater UI (MIT, © Lloyd Newton).
