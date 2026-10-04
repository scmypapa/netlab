#!/usr/bin/env bash
set -euo pipefail
source "$(dirname -- "${BASH_SOURCE[0]}")/install-common.sh"
[[ ! -e /opt/netlab/current ]] || { echo '已有正式安装，版本更新从面板执行。' >&2; exit 1; }
config=${1:-/etc/netlab/controller.env}
address=${2:-$(ip -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')}
apt-get update
apt-get install -y ca-certificates jq sudo openssl postgresql ovn-central
getent passwd netlab >/dev/null || useradd --system --home-dir /var/lib/netlab --shell /usr/sbin/nologin netlab
install -d -m 0750 -o root -g netlab /etc/netlab /etc/netlab/pki
install -d -m 0750 -o netlab -g netlab /var/lib/netlab /var/lib/netlab/update /var/lib/netlab/metrics
if [[ ! -f /etc/netlab/pki/ca.crt ]]; then
  umask 077
  openssl req -x509 -newkey rsa:3072 -nodes -days 3650 -subj '/CN=Netlab CA' -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' -keyout /etc/netlab/pki/ca.key -out /etc/netlab/pki/ca.crt
  openssl req -new -newkey rsa:3072 -nodes -subj '/CN=Netlab controller' -keyout /etc/netlab/pki/controller.key -out /etc/netlab/pki/controller.csr
  openssl x509 -req -in /etc/netlab/pki/controller.csr -CA /etc/netlab/pki/ca.crt -CAkey /etc/netlab/pki/ca.key -set_serial "0x$(openssl rand -hex 16)" -days 365 -extfile <(printf '%s\n' 'basicConstraints=critical,CA:FALSE' 'keyUsage=critical,digitalSignature,keyEncipherment' 'extendedKeyUsage=clientAuth') -out /etc/netlab/pki/controller.crt
  rm -- /etc/netlab/pki/controller.csr
fi
chown root:netlab /etc/netlab/pki/ca.crt /etc/netlab/pki/controller.crt /etc/netlab/pki/controller.key
chmod 0640 /etc/netlab/pki/ca.crt /etc/netlab/pki/controller.crt /etc/netlab/pki/controller.key
systemctl enable --now postgresql ovn-central
bash "$package/scripts/issue-node-certificate.sh" "$address" /etc/netlab/pki/ovn
ovn-nbctl set-ssl /etc/netlab/pki/ovn/node.key /etc/netlab/pki/ovn/node.crt /etc/netlab/pki/ovn/ca.crt
ovn-sbctl set-ssl /etc/netlab/pki/ovn/node.key /etc/netlab/pki/ovn/node.crt /etc/netlab/pki/ovn/ca.crt
ovn-nbctl set-connection "pssl:6641:$address"
ovn-sbctl set-connection "pssl:6642:$address"
if [[ ! -f $config ]]; then
  umask 077
  password=$(openssl rand -hex 24)
  admin_password=$(openssl rand -hex 16)
  runuser -u postgres -- psql -v ON_ERROR_STOP=1 -c "CREATE ROLE netlab LOGIN PASSWORD '$password';"
  runuser -u postgres -- createdb --owner=netlab netlab
  printf 'NETLAB_DATABASE_URL=postgres://netlab:%s@127.0.0.1:5432/netlab?sslmode=disable\nNETLAB_ADMIN_PASSWORD=%s\nNETLAB_LISTEN=:8090\nNETLAB_METRICS_URL=http://127.0.0.1:8428\nNETLAB_NODE_CA=/etc/netlab/pki/ca.crt\nNETLAB_NODE_CERT=/etc/netlab/pki/controller.crt\nNETLAB_NODE_KEY=/etc/netlab/pki/controller.key\n' "$password" "$admin_password" > "$config"
fi
if [[ $config != /etc/netlab/controller.env ]]; then install -m 0640 -o root -g netlab -- "$config" /etc/netlab/controller.env; fi
cat >> /etc/netlab/controller.env <<'CONFIG'
NETLAB_INSTALL_DIR=/opt/netlab
NETLAB_DATA_DIR=/var/lib/netlab
NETLAB_WEB_DIR=/opt/netlab/current/web
CONFIG
chown root:netlab /etc/netlab/controller.env
chmod 0640 /etc/netlab/controller.env
install_release /opt/netlab
for unit in netlab-controller netlab-update netlab-metrics; do
  install -m 0644 -- "$package/deploy/systemd/$unit.service" "/etc/systemd/system/$unit.service"
done
visudo -cf "$package/deploy/sudoers/netlab-update"
install -m 0440 -- "$package/deploy/sudoers/netlab-update" /etc/sudoers.d/netlab-update
systemctl daemon-reload
systemctl enable --now netlab-metrics.service
systemctl enable netlab-controller.service
systemctl restart netlab-controller.service
systemctl --no-pager status netlab-controller.service
printf '面板：http://%s:8090\n管理员：admin，初始密码保存在 /etc/netlab/controller.env\n' "$address"
