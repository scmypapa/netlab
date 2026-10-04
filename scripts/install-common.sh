#!/usr/bin/env bash
set -euo pipefail
[[ $(id -u) == 0 ]] || { echo '以 root 执行安装。' >&2; exit 1; }
source /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == 24.04 && $(uname -m) == x86_64 ]] || { echo '发布包安装目标：Ubuntu 24.04 x86-64。' >&2; exit 1; }
package=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
export DEBIAN_FRONTEND=noninteractive

install_release() {
  local root=$1
  local version
  version=$(jq -er '.version' "$package/release.json")
  [[ $version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
  [[ $(jq -r '.os + "/" + .arch' "$package/release.json") == linux/amd64 ]]
  install -d -m 0755 "$root/releases"
  local destination=$root/releases/$version
  mkdir -- "$destination"
  cp -a -- "$package/." "$destination/"
  chown -R root:root -- "$destination"
  chmod 0755 "$destination" "$destination/netlab-controller" "$destination/netlab-node"
  ln -s -- "$destination" "$root/current"
}
