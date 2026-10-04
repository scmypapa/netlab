#!/bin/bash
set -euo pipefail
source_dir=$1
prefix=$2
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
make install
