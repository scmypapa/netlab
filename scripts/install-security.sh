#!/usr/bin/env bash
set -euo pipefail
package=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if ! command -v suricata >/dev/null || ! command -v suricata-update >/dev/null; then
  fresh=false
  command -v suricata >/dev/null || fresh=true
  apt-get update
  apt-get install -y suricata suricata-update
  if $fresh; then systemctl disable --now suricata.service; fi
fi
if [[ ! -s /var/lib/suricata/rules/suricata.rules ]]; then
  suricata-update --no-reload
fi
for unit in netlab-suricata-rules.service netlab-suricata-rules.timer; do
  install -m 0644 -- "$package/deploy/systemd/$unit" "/etc/systemd/system/$unit"
done
systemctl daemon-reload
systemctl enable --now netlab-suricata-rules.timer
