# i2cshim — a fake I²C bus for a stock meshtasticd

RepeaterTastic hosts several Meshtastic identities on one radio, and each one is a
stock, unpatched `meshtasticd`. We want one real sensor on the host to appear to as
many of those identities as we like, each publishing it as **its own** sensor: on
its own schedule, in its own packets, answering directed telemetry requests itself.

The only way to do that without patching the firmware is to let the node believe it
owns the hardware. So RepeaterTastic sends no telemetry at all. It samples a source
once, writes the readings into each node's directory, and this shim — an
`LD_PRELOAD` object inside that node's container — answers the node's I²C traffic
from that file. meshtasticd opens a device path that does not exist in the kernel,
scans it, finds "chips", initialises their real drivers and reads them forever.
Every byte it reads is synthesised here.

`internal/sensors` is the other half: it decides which chips a node needs, writes
the values file, and shares the file format with this shim.

## Building

```sh
./build.sh          # or: make shim       (from the repo root)
```

Produces `build/i2cshim-linux-amd64.so` and `build/i2cshim-linux-arm64.so`.

The compile happens inside `debian:trixie` (via a small cached builder image), and
arm64 is cross-compiled — no qemu. The build asserts that neither object requires a
glibc symbol newer than **GLIBC_2.34**, so one object loads on every meshtasticd
image we support, and that the interposers are really exported.

## Testing

```sh
./test.sh           # or: make shim-test
./test.sh -v        # with a trace of every bus operation
```

`replay.c` is a faithful copy of Portduino's `LinuxHardwareI2C` plus the firmware's
112-address scan and its `getRegisterValue()`, Adafruit_BusIO's combined
write-then-read, the AHT10 command sequence and the PMSA003I frame read. It asserts
the decoded readings against the values file, rewrites the file and asserts the new
ones. No containers and no meshtasticd, about a second, non-zero exit on failure —
so it runs in CI.

## Environment

Set these per node, on the meshtasticd process:

| Variable | Meaning |
| --- | --- |
| `LD_PRELOAD` | absolute path to the `.so`, inside the container |
| `I2CSHIM_DEV` | comma-separated device paths to fake. Must match `I2C: I2CDevice:` in that node's `config.yaml`, and must **not** exist in the kernel |
| `I2CSHIM_CHIPS` | comma-separated chips to present, e.g. `pct2075,aht10,pmsa003i` (default `pct2075`) |
| `I2CSHIM_VALUES` | the readings file, re-read on every access |
| `I2CSHIM_LINEBUF` | `1` line-buffers stdout, so `docker logs` is prompt (debugging only) |
| `I2CSHIM_DEBUG` | `1` traces every bus operation to stderr |

A node also needs the matching telemetry modules enabled in its own config —
`telemetry.environment_measurement_enabled` for temperature/humidity/voltage, and
`telemetry.air_quality_enabled` for the PM values. The air-quality module only
attaches its sensors when it is already enabled at start-up, so enable it and then
restart the node.

## Chips and fields

Which chip carries which field is fixed, and mirrors `sensors.Chips` in
`internal/sensors/api.go`: the firmware merges every sensor into one packet and the
last writer wins, so exactly one emulated chip carries each field.

| Chip | Addr | Fields (values-file keys) | Telemetry it reaches | How it is detected |
| --- | --- | --- | --- | --- |
| `pct2075` | 0x37 | `temperature` | environment `temperature` | bare 1-byte read in the 0x30–0x37 probe range |
| `mcp9808` | 0x18 | `temperature` | environment `temperature` | device ID 0x07 must read 0x0400 |
| `ina226` | 0x40 | `voltage`, `current` | power / environment | MFG 0xFE = 0x5449 **and** die 0xFF = 0x2260 |
| `aht10` | 0x38 | `humidity` (+ `temperature`) | environment `relative_humidity` | address alone, no ID check |
| `pmsa003i` | 0x12 | `pm10`, `pm25`, `pm100` | air quality `pm*_standard` and `pm*_environmental` | address alone, no ID check |

Use `pct2075` **or** `mcp9808`, never both. `aht10` also reports `temperature`,
because the firmware's AHT10 driver fills each metric only `if (!has_*)` — whichever
temperature source the firmware happens to reach first, the reading is the file's.

`pm10` is PM1.0, `pm25` is PM2.5 and `pm100` is PM10, exactly as Meshtastic names
them. The shim puts the same three values in the frame's standard and environmental
slots; the particle counts are zero, as we have no source for them.

## The values file

One `field=value` per line. `field: value` and JSON-ish `"field": value,` are
accepted too, `#` comments and unknown keys are ignored, and a malformed or missing
file never fails a transfer — the chip just reports its default. The file is re-read
on **every** register access, so a new value is live on the node's next read with
nothing restarted.

```
temperature = 21.5
humidity = 64.5
pm10 = 12
pm25 = 34
pm100 = 56
voltage = 4.05
current = 0.15
```

Field names and units are exactly `sensors.Field` in `internal/sensors/api.go`
(°C, %, hPa, lx, V, **A**, µg/m³, mm, µR/h) and
`sensors.RenderValues` writes precisely this format. Keep the two in step: the Go
`ParseValues` and the C parser here are deliberately the same grammar.

## Two gotchas

**FORTIFY.** meshtasticd is built with `_FORTIFY_SOURCE`, so a `read()` whose length
glibc cannot prove safe — exactly Portduino's `::read(i2c_file, RXbuf, count)` — is
emitted as `__read_chk`, not `read`. Interpose `read()` alone and the I²C scan works
perfectly while every register read comes back as zeroes. We interpose both, and
both `build.sh` and `test.sh` refuse an object that does not export `__read_chk`.

**The register pointer survives a repeated START.** Portduino's `getRegisterValue()`
does `write(reg)` + `endTransmission(true)`, and then `requestFrom()` re-issues
`ioctl(I2C_SLAVE)` before a bare `read()`. A real chip keeps its register pointer
across that, so the shim only invalidates the pointer when the target address
actually changes. Reset it on every `I2C_SLAVE` and every register read on the
separate write+read path returns register 0.

Both read paths have to work: the combined `I2C_RDWR` transfer (Adafruit_BusIO's
`write_then_read`) and the separate `write()` + `read()` pair.

## Files

| File | |
| --- | --- |
| `i2cshim.c` | the shim; the header comment is the full design note |
| `replay.c` | the self-test's replay of Portduino's call sequences |
| `build.sh` | cross-builds both architectures in `debian:trixie` |
| `test.sh` | builds (or reuses) and runs the self-test |
