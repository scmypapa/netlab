#!/bin/sh
set -eu
systemctl enable --now openvswitch-switch ovn-central ovn-host
encap_ip=$(ip -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')
ovs-vsctl --may-exist add-br br-int
ovs-vsctl set Open_vSwitch . external_ids:ovn-remote=unix:/run/ovn/ovnsb_db.sock external_ids:ovn-encap-type=geneve external_ids:ovn-encap-ip="$encap_ip"
ip link set br-int up
