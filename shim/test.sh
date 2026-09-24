#!/bin/sh
# Self-test for i2cshim: replays Portduino's exact call sequences against the
# shim and asserts the decoded readings. No containers, no meshtasticd, about a
# second -- so it runs in CI.
#
# It prefers the artefact build.sh produced for this architecture; if there is
# none it compiles i2cshim.c with the host compiler, which tests the same source.
#
# If the host happens to have qemu-user and the armhf cross toolchain, it then
# runs the whole thing again against build/i2cshim-linux-arm.so, because 32-bit
# has its own traps (see __ioctl_time64 in i2cshim.c). Without them that stage is
# skipped with a line saying so -- it is a bonus, never a requirement.
#
#   ./test.sh              # build (or reuse) and run
#   ./test.sh -v           # also trace every bus operation
set -eu

cd "$(dirname "$0")"

VERBOSE=0
[ "${1:-}" = "-v" ] && VERBOSE=1

case "$(uname -m)" in
x86_64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
armv7* | armv8l) ARCH=arm ;;
*) ARCH=$(uname -m) ;;
esac

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

DEV=/dev/i2c-selftest
CHIPS="${I2CSHIM_CHIPS:-pct2075,mcp9808,ina226,aht10,pmsa003i,bh1750,rcwl9620,cgradsens,dfrobot_rain,lps22,bmp280}"

# run <label> <so> <replay binary> [runner ...]
run_replay() {
    label=$1 so=$2 bin=$3
    shift 3
    nm -D --defined-only "$so" 2>/dev/null | grep -q ' __read_chk$' ||
        { echo "$so does not export __read_chk -- FORTIFY reads would bypass the shim" >&2; exit 1; }
    values=$WORK/sensor-$label.kv
    echo "== $label =="
    set +e
    LD_PRELOAD="$(readlink -f "$so")" \
        I2CSHIM_DEV="$DEV" \
        I2CSHIM_CHIPS="$CHIPS" \
        I2CSHIM_VALUES="$values" \
        I2CSHIM_DEBUG="$VERBOSE" \
        ${1+"$@"} "$bin" "$DEV" "$values"
    rc=$?
    set -e
    [ "$rc" -eq 0 ] || { echo "FAILED: $label (exit $rc)" >&2; exit "$rc"; }
}

# ---- native --------------------------------------------------------------
SO=build/i2cshim-linux-$ARCH.so
if [ -f "$SO" ] && [ "$SO" -nt i2cshim.c ]; then
    echo "using $SO"
else
    SO=$WORK/i2cshim.so
    echo "compiling i2cshim.c with $(command -v "${CC:-cc}")"
    ${CC:-cc} -std=gnu11 -shared -fPIC -O2 -Wall -Wextra -Werror \
        -o "$SO" i2cshim.c -ldl -lpthread -lm
fi

# Built the way meshtasticd is: FORTIFY on, so the register reads really do go
# through __read_chk and not read().
echo "compiling replay.c (FORTIFY on, as meshtasticd is built)"
${CC:-cc} -std=gnu11 -O2 -D_FORTIFY_SOURCE=2 -Wall -Wextra -Werror \
    -o "$WORK/replay" replay.c -lm

run_replay "native ($ARCH)" "$SO" "$WORK/replay"

# ---- armv7 under qemu-user, when the host can ----------------------------
ARM_SO=build/i2cshim-linux-arm.so
ARM_CC=${ARM_CC:-arm-linux-gnueabihf-gcc}
ARM_SYSROOT=${I2CSHIM_ARM_SYSROOT:-/usr/arm-linux-gnueabihf}
QEMU_ARM=$(command -v qemu-arm 2>/dev/null || command -v qemu-arm-static 2>/dev/null || true)

if [ "$ARCH" = arm ]; then
    : # already covered natively
elif [ ! -f "$ARM_SO" ]; then
    echo "skipping the armv7 stage: no $ARM_SO (run ./build.sh)"
elif [ -z "$QEMU_ARM" ]; then
    echo "skipping the armv7 stage: no qemu-arm on this host"
elif ! command -v "$ARM_CC" >/dev/null 2>&1; then
    echo "skipping the armv7 stage: no $ARM_CC to build the replay with"
elif [ ! -d "$ARM_SYSROOT" ]; then
    echo "skipping the armv7 stage: no armhf sysroot at $ARM_SYSROOT"
else
    echo "compiling replay.c for armv7 with $ARM_CC"
    "$ARM_CC" -std=gnu11 -O2 -D_FORTIFY_SOURCE=2 -Wall -Wextra -Werror \
        -o "$WORK/replay-arm" replay.c -lm
    run_replay "armv7 under $(basename "$QEMU_ARM")" "$ARM_SO" "$WORK/replay-arm" \
        "$QEMU_ARM" -L "$ARM_SYSROOT"
fi

echo "PASS"
