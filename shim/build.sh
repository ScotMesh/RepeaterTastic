#!/bin/sh
# Build i2cshim.so for every architecture `make dist` ships a daemon for.
#
# It has to be built against a glibc no newer than the oldest meshtasticd image
# we support, and the host distribution is usually far newer, so the compile
# happens inside debian:trixie. The two ARM targets are cross-compiled -- no
# qemu, no emulation.
#
# The toolchain lives in a small cached image so the actual compile runs as the
# invoking user and never leaves root-owned files behind. Output, named for the
# Go GOARCH the daemon embeds it under:
#
#   build/i2cshim-linux-amd64.so
#   build/i2cshim-linux-arm64.so
#   build/i2cshim-linux-arm.so     (armv7 hard-float; GOARCH=arm, GOARM=7)
#
# There is deliberately no armv6 object: GOARCH=arm here means armv7. A Pi Zero
# or Pi 1 gets no shim and the daemon says so plainly instead of half-working.
#
# Reproducible as far as gcc lets us: no build-id, no .comment, the source path
# mapped to a fixed name, and SOURCE_DATE_EPOCH honoured if set.
set -eu

cd "$(dirname "$0")"

IMAGE=${I2CSHIM_BUILDER_IMAGE:-i2cshim-builder:trixie}
BASE=${I2CSHIM_BASE_IMAGE:-debian:trixie}
MAX_GLIBC=2.34 # keep the requirement at or below this
TARGETS=${I2CSHIM_TARGETS:-"amd64 arm64 arm"}

if ! command -v docker >/dev/null 2>&1; then
    echo "build.sh needs docker (the compile must happen inside $BASE)" >&2
    exit 1
fi

# The toolchain image. Always asked for, so adding a cross compiler here can
# never leave a stale image behind; docker caches it, so it costs nothing.
echo "==> toolchain image $IMAGE"
docker build -q -t "$IMAGE" -f - . >/dev/null <<EOF
FROM $BASE
RUN apt-get update -qq \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
      gcc libc6-dev binutils \
      gcc-aarch64-linux-gnu libc6-dev-arm64-cross \
      gcc-arm-linux-gnueabihf libc6-dev-armhf-cross \
 && rm -rf /var/lib/apt/lists/*
EOF

mkdir -p build

CFLAGS="-std=gnu11 -shared -fPIC -O2 -Wall -Wextra -Werror -fvisibility=default"
CFLAGS="$CFLAGS -fno-ident -ffile-prefix-map=/src=. -Wl,--build-id=none -Wl,-z,relro,-z,now"
LIBS="-ldl -lpthread -lm"

echo "==> compiling in $IMAGE"
docker run --rm \
    --user "$(id -u):$(id -g)" \
    -v "$PWD:/src" -w /src \
    -e "SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-0}" \
    -e "MAX_GLIBC=$MAX_GLIBC" \
    -e "TARGETS=$TARGETS" \
    "$IMAGE" sh -eu -c '
    for target in $TARGETS; do
        EXTRA=
        case $target in
        amd64)
            TRIPLE= ;;
        arm64)
            TRIPLE=aarch64-linux-gnu- ;;
        arm)
            # Debian armhf: armv7-a, VFPv3-D16, hard float. Spelled out so this
            # can never quietly become an armv6 object.
            TRIPLE=arm-linux-gnueabihf-
            EXTRA="-march=armv7-a -mfpu=vfpv3-d16 -mfloat-abi=hard" ;;
        *)
            echo "unknown target $target" >&2; exit 1 ;;
        esac
        CC=${TRIPLE}gcc
        STRIP=${TRIPLE}strip
        READELF=${TRIPLE}readelf

        out=build/i2cshim-linux-$target.so
        $CC '"$CFLAGS"' $EXTRA -o "$out" i2cshim.c '"$LIBS"'
        $STRIP --strip-unneeded --remove-section=.comment "$out"

        # Every glibc version this object demands must be <= MAX_GLIBC, or it
        # will refuse to load on an older image. This is how we catch a careless
        # atoi()/strtol() picking up __isoc23_strtol.
        worst=$($READELF -V "$out" | tr -s " " "\n" | sed -n "s/^GLIBC_//p" | sort -V | tail -1)
        if [ "$(printf "%s\n%s\n" "$worst" "$MAX_GLIBC" | sort -V | tail -1)" != "$MAX_GLIBC" ]; then
            echo "$out needs GLIBC_$worst, newer than $MAX_GLIBC" >&2
            $READELF -V "$out" | grep -o "GLIBC_[0-9.]*" | sort -Vu >&2
            exit 1
        fi
        # The interposers that matter must really be exported. __read_chk above
        # all: without it a FORTIFY-hardened meshtasticd reads only zeroes.
        SYMS="read __read_chk ioctl open open64 openat openat64 close write"
        # 32-bit ARM builds everything with _TIME_BITS=64, so the callers ask for
        # __ioctl_time64 and never for ioctl. Missing it breaks the whole bus.
        [ "$target" = arm ] && SYMS="$SYMS __ioctl_time64"
        for sym in $SYMS; do
            $READELF --dyn-syms -W "$out" | grep -q " $sym\$" || { echo "$out does not export $sym" >&2; exit 1; }
        done
        machine=$($READELF -h "$out" | sed -n "s/^  Machine: *//p")
        echo "    $out ($machine, GLIBC_$worst)"
    done
'

for t in $TARGETS; do
    ls -l "build/i2cshim-linux-$t.so"
done
echo "==> ok"
