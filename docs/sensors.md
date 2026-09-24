# Sensors

[← README](../README.md) · [Configuration](configuration.md) · [Web GUI](web-gui.md) · [HTTP API](api.md) · [Hosted nodes](meshtasticd-nodes.md) · [Architecture](architecture.md)

A sensor plugged into the machine running RepeaterTastic can be published by any of your identities —
as many as you like — with each one broadcasting it as **its own** sensor. A thermometer in a shed
can appear on three personas at once; an app connected to any of them sees a normal environment
reading from a normal node.

- [How it works](#how-it-works)
- [Where a reading comes from](#where-a-reading-comes-from): a command, a file, or a push
- [Which fields can be published](#which-fields-can-be-published)
- [Setting it up in the GUI](#setting-it-up-in-the-gui)
- [Setting it up in the config file](#setting-it-up-in-the-config-file)
- [Pushing readings from a plugin or a script](#pushing-readings-from-a-plugin-or-a-script)
- [Airtime](#airtime)
- [How the shim works](#how-the-shim-works) and [what to check when it doesn't](#troubleshooting)

## How it works

**RepeaterTastic never sends a telemetry packet.** It couldn't do it honestly — the packet has to come
from the identity that owns the sensor, signed and scheduled by that node.

So the reading is handed to the node instead. Each hosted identity is a real `meshtasticd`
([hosted nodes](meshtasticd-nodes.md)), and a small shim inside it answers that node's I²C reads from
a file RepeaterTastic keeps up to date. The node scans its I²C bus at start-up, finds what it thinks
is a temperature chip, and from then on reads it, broadcasts it on its own telemetry schedule, and
answers telemetry requests — entirely by itself, with no involvement from us at the moment it
transmits.

```
  host sensor ──► RepeaterTastic ──► <node dir>/sensors.values ──► shim ──► meshtasticd ──► on air
   (command,        samples it       one file per identity      pretends      reads it,
    file or          once                                       to be I²C     broadcasts it
    push)
```

That is the whole trick, and it has three consequences worth knowing:

- **One reading, many identities.** Every attached identity gets its own copy of the file, so the
  same sensor can be published by as many nodes as you want, each with its own node id, telemetry
  interval and channel.
- **The node is stock.** It's the official `meshtasticd` image or binary, unpatched. Nothing needs
  root, no kernel module is loaded, and no I²C hardware has to exist — the shim intercepts the calls
  before they reach the kernel.
- **A sensor must exist before the node boots.** `meshtasticd` scans for sensors once at start-up and
  then throws the scanner away. Attaching a sensor to a running identity therefore restarts that
  identity's node (the GUI says so before it does it). Changing a *value* needs no restart: the next
  read picks it up.

## Where a reading comes from

A **source** is one thing this host can read. Three kinds:

| Kind | What it does | Use it for |
| --- | --- | --- |
| `exec` | Runs a command on an interval and reads its stdout | anything with a CLI: `sensors`, a Python script for a 1-Wire probe, `rtl_433` |
| `file` | Reads a file on an interval | something else already writes the value: a cron job, Home Assistant, another daemon |
| `push` | Waits to be given readings over the HTTP API or by a plugin | irregular readings, a remote sensor, a plugin that owns the hardware |

`exec` and `file` sources both speak the same plain format — one `field=value` per line:

```
temperature=18.4
humidity=63.2
```

`field: value` and JSON-ish (`"temperature": 18.4,`) are accepted too, blank lines and `#` comments
are ignored, and an unknown field name is skipped rather than failing the read. A command that exits
non-zero, times out, or prints nothing usable leaves the last good reading in place and shows the
error in the GUI.

`scale` multiplies a field as it arrives, for a source that reports in another unit:

```yaml
scale: {current: 0.001}   # the script prints mA, Meshtastic wants A
```

### Who can add one, and what that means

An `exec` sensor runs a command as the user RepeaterTastic runs as, and a `file` sensor reads any
file that user can read. So adding a sensor is as privileged as editing the config file: anyone who
can sign in to the GUI, **or who holds an API token**, can run commands on the host. API tokens are
not scoped — a token you gave Home Assistant for reading statistics can do this too.

That is the same level of trust the GUI already needs (it installs plugins and restores backups), but
it is worth knowing before you hand a token out. If you want a sensor without granting that, use a
`push` sensor: it has nothing to run, and whatever feeds it needs no access to the host.

## Which fields can be published

Meshtastic telemetry has a fixed set of environment fields, and each one reaches the air by way of a
chip the shim imitates. Which chip carries which field is fixed, because the firmware merges every
detected sensor into one packet and the last one to write a field wins — so exactly one imitated chip
owns each field.

| Field | Unit | Imitated chip | Address |
| --- | --- | --- | --- |
| `temperature` | °C | PCT2075 | 0x37 |
| `humidity` | % | AHT10 | 0x38 |
| `pm10` `pm25` `pm100` | µg/m³ | PMSA003I | 0x12 |
| `voltage` `current` | V, A | INA226 | 0x40 |

Those four are the chips the shim imitates today, each proven on a stock meshtasticd. Anything
Meshtastic can carry but they can't — `pressure`, `lux`, `distance`, `radiation`, `rainfall_1h`,
`rainfall_24h` — is refused with a message saying so, rather than quietly dropped. Adding one is a
chip model in `shim/i2cshim.c` and a row in the table in `internal/sensors/api.go`; `shim/README.md`
explains how, and the self-test is where you prove it.

Pick the fields per attachment: a source that reports temperature and humidity can be published
whole on one identity and temperature-only on another.

Three notes from the Meshtastic side:

- The phone apps' environment tab wants **temperature and humidity together**; temperature alone
  shows up, but in a plainer way.
- **Particulates are a separate telemetry packet.** A node publishing `pm*` is asked for air-quality
  telemetry as well as environment telemetry, and its first air-quality packet comes about two
  minutes after it starts — the firmware's own delay.
- `pressure` needs a BME280's calibration block emulated, which is more pretending than it's worth
  for now.

## Setting it up in the GUI

**Sensors** in the sidebar:

1. **Add sensor** — name it, pick `exec`, `file` or `push`, give the command or path, set how often to
   read it. **Read now** tries it immediately and shows exactly what came back, so a typo in a
   command is obvious before you attach it to anything.
2. The list shows each sensor's latest reading, its age, and the last error if it's failing.
3. **Attach** — tick the identities that should publish it and the fields each should send. Attaching
   to a running identity restarts that identity's node; the dialog tells you which ones will bounce.

An identity's drawer has a **Sensors** section showing what it publishes, so you can also work the
other way round: start from a persona and give it sensors.

Everything the GUI does is written back to `repeatertastic.yaml`. There is no separate store.

## Setting it up in the config file

The same feature, no browser needed:

```yaml
sensors:
    interval: 1h              # how often each identity broadcasts environment telemetry; min 30m
    sources:
        - id: shed            # names it in the GUI, the API and attach below
          name: Shed
          kind: exec
          command: /usr/local/bin/read-shed
          interval: 5m        # how often we read it; at least 5s
        - id: rack
          name: Rack PSU
          kind: file
          path: /run/rack-psu.values
          interval: 30s
          scale: {current: 0.001}
        - id: weather
          kind: push
    attach:
        - sensor: shed
          identities: [BASE, "!a1c40e07"]   # short names, node ids, or [all]
          fields: [temperature, humidity]   # empty = everything the source reports
        - sensor: rack
          identities: [all]
          fields: [voltage, current]
```

`identities` matches an identity's short name (case doesn't matter), its node id, or `all` for every
identity on the host. A sensor attached to `all` reaches identities you add later, too.

`interval` is what each hosted node is asked to use for environment telemetry. The firmware's own
floor is 30 minutes and RepeaterTastic refuses anything shorter.

## Pushing readings from a plugin or a script

A `push` source is read-only until something gives it a value:

```
POST /api/sensors/shed/push
{"temperature": 18.4, "humidity": 63.2}
```

Fields are the names in the table above. See [HTTP API → Sensors](api.md#sensors) for the full set of
endpoints.

A plugin does the same over the Plugin API with the `sensors.publish` permission — `PublishSensor`,
or `c.PublishSensor(ctx, "weather", map[string]float64{"temperature": 18.4})` with the Go SDK. That's
how a plugin that owns some hardware, or fetches a reading from a service, gets it on air; it still
can't create a sensor or choose who publishes it. See
[Plugin API → ListSensors and PublishSensor](plugin-api.md#listsensors-and-publishsensor).

## Airtime

Environment telemetry is one small packet per identity per interval, but it is one *per identity*: ten
identities publishing the same thermometer hourly is ten packets an hour. Every one of them is counted
in the site's airtime budget like any other transmission ([Radios](radios.md#airtime)), and the
firmware skips a telemetry broadcast when the channel is busy. If a mast is tight for airtime,
publish a sensor on the one identity people actually look at.

## How the shim works

`shim/` holds a small C library that RepeaterTastic loads into each hosted node with `LD_PRELOAD`. It
intercepts the handful of libc calls Portduino uses to talk to `/dev/i2c-*` and answers them as the
chip the node is looking for, reading the current value out of that node's values file. The file is
plain text and rewritten atomically, so a read never sees half a number.

`shim/README.md` has the details, including how it's built for amd64 and arm64 and the self-test that
replays the firmware's exact call sequences.

## Troubleshooting

**The node never sends telemetry.** Check the identity is actually hosted (`meshtasticd`, not a
virtual identity) and that it restarted after the sensor was attached. The node log line to look for
is the sensor being detected at start-up.

**It says 0.** A node has to be able to answer its I²C scan before the first sample arrives, so a
field with no reading yet reads as 0. An `exec` or `file` sensor fills in within its interval; a
`push` sensor reads 0 until something pushes to it.

**The value never changes.** RepeaterTastic writes the file only when the reading changes; check the
sensor's age in the GUI. An `exec` source that's failing keeps its last good value and shows the
error.

**Only one of two fields arrives.** Both fields have to be in that attachment's field list, and both
chips have to be carried — check the table above.
