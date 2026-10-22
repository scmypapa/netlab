#!/bin/bash
set -euo pipefail
source_dir=$1
destination=$2
mkdir -p "$destination"
destination=$(cd -- "$destination" && pwd)
prefix=/opt/netlab-node/current/guacamole
staging=$(mktemp -d)
trap 'rm -rf -- "$staging"' EXIT
commit=ab36756b596520ae2a94cd4d25e406e0b5aa820b
mkdir -p "$source_dir"
curl -fL "https://codeload.github.com/apache/guacamole-server/tar.gz/$commit" |
  tar -xz --strip-components=1 -C "$source_dir"
cd "$source_dir"
autoreconf -fi
CFLAGS="-O2 -Wno-error=deprecated-declarations" ./configure \
  --prefix="$prefix" --disable-static --disable-guacenc --disable-guaclog \
  --without-vnc --without-ssh --without-telnet --without-pulse
make -j"$(nproc)"
make DESTDIR="$staging" install
cp -a -- "$staging$prefix/." "$destination/"
runtime_libdir=$(pkg-config --variable=libdir freerdp-client3)
cp -L -- "$runtime_libdir"/libfreerdp-client3.so.3 "$runtime_libdir"/libfreerdp3.so.3 "$runtime_libdir"/libwinpr3.so.3 "$destination/lib/"
LD_LIBRARY_PATH="$destination/lib" ldd "$destination/lib/libguac-client-rdp.so" |
  awk '/not found/{missing=1} {print} END{exit missing}'
