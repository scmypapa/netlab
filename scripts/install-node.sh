#!/usr/bin/env bash
set -euo pipefail
source "$(dirname -- "${BASH_SOURCE[0]}")/install-common.sh"
[[ ! -e /opt/netlab-node/current ]] || { echo '已有正式安装，版本更新从主站面板执行。' >&2; exit 1; }
controller=${1:?用法：install-node.sh 主站IP 节点证书目录}
certificates=${2:?提供节点证书目录}
address=$(ip -4 route get "$controller" | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')
apt-get update
apt-get install -y ca-certificates jq containerd runc qemu-system-x86 qemu-utils qemu-block-extra ceph-common libvirt-daemon-system libvirt-clients ovmf swtpm swtpm-tools numad openvswitch-switch ovn-host genisoimage nftables iproute2 tshark e2fsprogs util-linux libfreerdp3-3 libwinpr3-3 libcairo2 libjpeg-turbo8 libpng16-16t64 libpango-1.0-0 libpangoft2-1.0-0 libwebp7 libssl3t64 libuuid1
install -d -m 0711 /var/lib/netlab-node
install -d -m 0700 /etc/netlab-node /var/lib/netlab-node/update
for file in ca.crt node.crt node.key; do install -m 0600 -- "$certificates/$file" "/etc/netlab-node/$file"; done
cat > /etc/netlab-node/node.env <<CONFIG
NETLAB_INSTALL_DIR=/opt/netlab-node
NETLAB_NODE_ARGS="--data /var/lib/netlab-node --ca /etc/netlab-node/ca.crt --cert /etc/netlab-node/node.crt --key /etc/netlab-node/node.key --ovn ssl:$controller:6641 --advertise-address $address"
CONFIG
chmod 0600 /etc/netlab-node/node.env
install_release /opt/netlab-node
systemctl enable --now containerd libvirtd openvswitch-switch ovn-host
ovs-vsctl --may-exist add-br br-int
ovs-vsctl set-ssl /etc/netlab-node/node.key /etc/netlab-node/node.crt /etc/netlab-node/ca.crt
ovs-vsctl set Open_vSwitch . "external_ids:ovn-remote=ssl:$controller:6642" external_ids:ovn-encap-type=geneve "external_ids:ovn-encap-ip=$address"
ip link set br-int up
for unit in netlab-node netlab-node-update netlab-guacd; do
  install -m 0644 -- "$package/deploy/systemd/$unit.service" "/etc/systemd/system/$unit.service"
done
systemctl daemon-reload
systemctl enable --now netlab-guacd.service
systemctl enable netlab-node.service
systemctl restart netlab-node.service
systemctl --no-pager status netlab-node.service
printf '节点地址：https://%s:9443\n在面板资源页登记此地址。\n' "$address"
