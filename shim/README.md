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

Produces one object per architecture the daemon ships for, named after the Go
`GOARCH` it is embedded under — that is what the embed lookup keys off:

| file | target |
| --- | --- |
| `build/i2cshim-linux-amd64.so` | x86-64 |
| `build/i2cshim-linux-arm64.so` | AArch64 (64-bit Pi OS) |
| `build/i2cshim-linux-arm.so` | **armv7** hard-float (`GOARCH=arm`, `GOARM=7`, 32-bit Pi OS) |

`make shim` also copies all three into `internal/nodes/shim/`, which the daemon
embeds, so hosting a sensor needs nothing installed on the host.

There is deliberately **no armv6 object**. `GOARCH=arm` here means armv7
(`-march=armv7-a -mfpu=vfpv3-d16 -mfloat-abi=hard`, Debian's armhf baseline), so a
Pi Zero or Pi 1 on an armv6 build has no shim at all and the daemon says so plainly
rather than starting a node that half-works.

The compile happens inside `debian:trixie` (via a small cached builder image), and
both ARM targets are cross-compiled — no qemu. The build asserts that no object
requires a glibc symbol newer than **GLIBC_2.34**, so one object loads on every
meshtasticd image we support, and that every interposer is really exported
(`__ioctl_time64` too, on 32-bit ARM — see below).

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

It runs the whole suite a second time against `build/i2cshim-linux-arm.so` under
`qemu-user`, **if** the host has `qemu-arm`, an `arm-linux-gnueabihf-gcc` to build
the replay with, and an armhf sysroot. When it doesn't, that stage prints one line
saying which piece is missing and the native run stands on its own. 32-bit is worth
the trouble: it is where `__ioctl_time64` bit us.

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
| `bmp280` | 0x76 | `pressure` (+ `temperature`) | environment `barometric_pressure` | chip id 0xD0 = 0x58, and register 0x00 for the scan's fallback |
| `bh1750` | 0x23 | `lux` | environment `lux` | 0x86 must not be an LTR553ALS, then a power-on write is acknowledged |
| `rcwl9620` | 0x57 | `distance` | environment `distance` | 0xFF must not be 0x15 (that would be a MAX30102) |
| `dfrobot_rain` | 0x1D | `rainfall_1h`, `rainfall_24h` | environment `rainfall_1h` / `rainfall_24h` | 0xF0 without DS2482 status bits, then vid 0x3343 / pid 0x100C0 at 0x00 |
| `cgradsens` | 0x66 | `radiation` — **do not use, see below** | environment `radiation` | product id 0x00 = 0x7D |
| `lps22` | 0x5C | `pressure` (+ `temperature`) | nothing: no driver in the Linux build | WHO_AM_I 0x0F = 0xB1 |

`bmp280` carries pressure by making Bosch's compensation polynomial the identity:
with `dig_T1 = 0, dig_T2 = 16384, dig_T3 = 0` the firmware computes `degC = adc_T /
5120`, and with `dig_P1 = 6250` and every other pressure coefficient zero it computes
`Pa = 1048576 - adc_P`. Both invert to exact integers, so what the node broadcasts is
what the values file said.

Two chips are present but not offered by `internal/sensors`:

- **`cgradsens`** works on the wire — the self-test proves the right bytes — but
  Portduino keeps received bytes in a `char RXbuf[1000]` and returns them through
  `int tmpVal = RXbuf[RXindex]`, so any byte over 0x7F arrives negative. Drivers that
  store into a `uint8_t` first (RCWL9620) are fine; `CGRadSensSensor` assigns straight
  into a `uint32_t`, so 13.7 µR/h reaches the mesh as 429496736. `check_signed_byte`
  in `replay.c` pins this down, and will start failing when it is fixed upstream.
- **`lps22`** answers correctly, but the meshtasticd Debian package is built without
  `Adafruit_LPS2X`, so nothing ever reads it. BMP280 is there instead.

Use `pct2075` **or** `mcp9808`, never both, and `bmp280` **or** `lps22`, never both. `aht10` also reports `temperature`,
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

## And one more, on 32-bit ARM

**`ioctl` is not called `ioctl`.** On a glibc port whose `time_t` is still 32 bits —
Debian armhf and i386 — everything is compiled with `_TIME_BITS=64` by default, and
`<sys/ioctl.h>` then redirects `ioctl()` to `__ioctl_time64`. meshtasticd's armhf
package is built that way, so it never calls the symbol `ioctl` at all: interpose
only `ioctl` and the fake bus opens, the scan finds nothing, and every read NAKs.
The shim defines both, sharing one body — the variadic third argument is read
identically — and `build.sh` requires `__ioctl_time64` in the arm object.

The same defaults rename `open`/`openat` to `open64`/`openat64`, which collided with
the shim's own `open64`, so `i2cshim.c` turns the large-file redirection off before
its first include. No interposed signature here takes an `off_t` or a `time_t`, so
`size_t`/`ssize_t` widths are the only thing that varies and those follow the
platform's own prototypes.

## Files

| File | |
| --- | --- |
| `i2cshim.c` | the shim; the header comment is the full design note |
| `replay.c` | the self-test's replay of Portduino's call sequences |
| `build.sh` | cross-builds both architectures in `debian:trixie` |
| `test.sh` | builds (or reuses) and runs the self-test |
