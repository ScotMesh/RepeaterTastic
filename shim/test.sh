#!/bin/sh
# Self-test for i2cshim: replays Portduino's exact call sequences against the
# shim and asserts the decoded readings. No containers, no meshtasticd, about a
# second -- so it runs in CI.
#
# It prefers the artefact build.sh produced for this architecture; if there is
# none it compiles i2cshim.c with the host compiler, which tests the same source.
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
*) ARCH=$(uname -m) ;;
esac

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

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

nm -D --defined-only "$SO" 2>/dev/null | grep -q ' __read_chk$' ||
    { echo "$SO does not export __read_chk -- FORTIFY reads would bypass the shim" >&2; exit 1; }

DEV=/dev/i2c-selftest
VALUES=$WORK/sensor.kv

set +e
LD_PRELOAD="$(readlink -f "$SO")" \
    I2CSHIM_DEV="$DEV" \
    I2CSHIM_CHIPS=pct2075,mcp9808,ina226,aht10,pmsa003i \
    I2CSHIM_VALUES="$VALUES" \
    I2CSHIM_DEBUG="$VERBOSE" \
    "$WORK/replay" "$DEV" "$VALUES"
rc=$?
set -e

if [ "$rc" -ne 0 ]; then
    echo "FAILED (exit $rc)" >&2
    exit "$rc"
fi
echo "PASS"
