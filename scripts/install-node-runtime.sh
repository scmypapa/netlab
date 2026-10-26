#!/usr/bin/env bash
set -euo pipefail
package=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
export DEBIAN_FRONTEND=noninteractive

packages=()
command -v modprobe >/dev/null || packages+=(kmod)
if ! modinfo vrf >/dev/null 2>&1; then
  packages+=("linux-modules-extra-$(uname -r)")
fi
# Virtual kernel images omit extra modules. Their standard extra metapackage
# follows future kernel updates without adding headers or changing kernel series.
while read -r status virtual version; do
  [[ $status == installed ]] || continue
  extra=${virtual/linux-image-virtual/linux-image-extra-virtual}
  if read -r extra_status extra_version < <(dpkg-query -W -f='${db:Status-Status} ${Version}\n' "$extra" 2>/dev/null) &&
     [[ $extra_status == installed ]] && dpkg --compare-versions "$extra_version" ge "$version"; then
    continue
  fi
  packages+=("$extra=$version")
done < <(dpkg-query -W -f='${db:Status-Status} ${Package} ${Version}\n' 'linux-image-virtual*' 2>/dev/null)

fresh=false
if ! command -v suricata >/dev/null || ! command -v suricata-update >/dev/null; then
  command -v suricata >/dev/null || fresh=true
  packages+=(suricata suricata-update)
fi
if ((${#packages[@]})); then
  apt-get update
  apt-get install -y "${packages[@]}"
fi
if $fresh; then systemctl disable --now suricata.service; fi
modprobe vrf
install -D -m 0644 /dev/stdin /etc/modules-load.d/netlab-network.conf <<< vrf

if [[ ! -s /var/lib/suricata/rules/suricata.rules ]]; then
  suricata-update --no-reload
fi
for unit in netlab-suricata-rules.service netlab-suricata-rules.timer; do
  install -m 0644 -- "$package/deploy/systemd/$unit" "/etc/systemd/system/$unit"
done
systemctl daemon-reload
systemctl enable --now netlab-suricata-rules.timer
