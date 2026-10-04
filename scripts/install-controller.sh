#!/usr/bin/env bash
set -euo pipefail

if [[ $(id -u) != 0 ]]; then
  echo '以 root 执行安装。' >&2
  exit 1
fi
package=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
config=${1:?用法：install-controller.sh /path/controller.env}
version=$(jq -er '.version' "$package/release.json")
[[ $version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
[[ $(jq -r '.os + "/" + .arch' "$package/release.json") == linux/amd64 ]]
[[ $(uname -m) == x86_64 ]]
getent passwd netlab >/dev/null || useradd --system --home-dir /var/lib/netlab --shell /usr/sbin/nologin netlab
install -d -m 0755 /opt/netlab/releases /etc/netlab
install -d -m 0750 -o netlab -g netlab /var/lib/netlab /var/lib/netlab/update
destination=/opt/netlab/releases/$version
mkdir -- "$destination"
cp -a -- "$package/." "$destination/"
chown -R root:root -- "$destination"
chmod 0755 "$destination" "$destination/netlab-controller" "$destination/netlab-node"
install -m 0640 -o root -g netlab -- "$config" /etc/netlab/controller.env
cat >> /etc/netlab/controller.env <<'CONFIG'
NETLAB_INSTALL_DIR=/opt/netlab
NETLAB_DATA_DIR=/var/lib/netlab
NETLAB_WEB_DIR=/opt/netlab/current/web
CONFIG
install -m 0644 -- "$package/deploy/systemd/netlab-controller.service" /etc/systemd/system/netlab-controller.service
install -m 0644 -- "$package/deploy/systemd/netlab-update.service" /etc/systemd/system/netlab-update.service
visudo -cf "$package/deploy/sudoers/netlab-update"
install -m 0440 -- "$package/deploy/sudoers/netlab-update" /etc/sudoers.d/netlab-update
ln -s -- "$destination" /opt/netlab/.current-install
mv -Tf -- /opt/netlab/.current-install /opt/netlab/current
systemctl daemon-reload
systemctl enable netlab-controller.service netlab-update.service
systemctl restart netlab-controller.service
systemctl --no-pager status netlab-controller.service
