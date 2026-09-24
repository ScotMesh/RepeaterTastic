#!/bin/sh
# Build i2cshim.so for linux/amd64 and linux/arm64.
#
# It has to be built against a glibc no newer than the oldest meshtasticd image
# we support, and the host distribution is usually far newer, so the compile
# happens inside debian:trixie. arm64 is cross-compiled -- no qemu, no emulation.
#
# The toolchain lives in a small cached image so the actual compile runs as the
# invoking user and never leaves root-owned files behind. Output:
#
#   build/i2cshim-linux-amd64.so
#   build/i2cshim-linux-arm64.so
#
# Reproducible as far as gcc lets us: no build-id, no .comment, the source path
# mapped to a fixed name, and SOURCE_DATE_EPOCH honoured if set.
set -eu

cd "$(dirname "$0")"

IMAGE=${I2CSHIM_BUILDER_IMAGE:-i2cshim-builder:trixie}
BASE=${I2CSHIM_BASE_IMAGE:-debian:trixie}
MAX_GLIBC=2.34 # keep the requirement at or below this

if ! command -v docker >/dev/null 2>&1; then
    echo "build.sh needs docker (the compile must happen inside $BASE)" >&2
    exit 1
fi

# The toolchain image. Cached after the first run; rebuild is a no-op.
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    echo "==> building toolchain image $IMAGE"
    docker build -t "$IMAGE" -f - . >/dev/null <<EOF
FROM $BASE
RUN apt-get update -qq \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
      gcc libc6-dev gcc-aarch64-linux-gnu libc6-dev-arm64-cross binutils \
 && rm -rf /var/lib/apt/lists/*
EOF
fi

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
    "$IMAGE" sh -eu -c '
    for target in amd64 arm64; do
        case $target in
        amd64) CC=gcc; STRIP=strip; READELF=readelf ;;
        arm64) CC=aarch64-linux-gnu-gcc; STRIP=aarch64-linux-gnu-strip; READELF=aarch64-linux-gnu-readelf ;;
        esac
        out=build/i2cshim-linux-$target.so
        $CC '"$CFLAGS"' -o "$out" i2cshim.c '"$LIBS"'
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
        # And the two interposers that matter must really be exported.
        for sym in read __read_chk ioctl open openat; do
            $READELF --dyn-syms -W "$out" | grep -q " $sym\$" || { echo "$out does not export $sym" >&2; exit 1; }
        done
        echo "    $out (GLIBC_$worst)"
    done
'

ls -l build/i2cshim-linux-amd64.so build/i2cshim-linux-arm64.so
echo "==> ok"
