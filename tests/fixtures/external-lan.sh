#!/usr/bin/env bash
set -euo pipefail

if [[ ${1:-} == stop ]]; then
  for name in native 100 200; do systemctl stop "nlt-lan-$name.service"; done
  ovs-vsctl --if-exists del-br nlt-ovs
  for name in nlt-uplink nlt-bport nlt-oport nlt-linux; do
    if ip link show "$name" >/dev/null 2>&1; then ip link delete "$name"; fi
  done
  for name in nlt-switch nlt-lan100 nlt-lan200; do ip netns delete "$name"; done
  exit
fi

ip netns add nlt-switch
ip -n nlt-switch link add br0 type bridge
ip -n nlt-switch link set br0 up
ip -n nlt-switch address add 192.0.2.1/24 dev br0
ip -n nlt-switch address add fd11::1/64 dev br0
ip -n nlt-switch link set lo up
ip link add nlt-uplink type veth peer name nlt-trunk
ip link set nlt-trunk netns nlt-switch
ip -n nlt-switch link set nlt-trunk master br0
ip -n nlt-switch link set nlt-trunk up
ip link set nlt-uplink up

for vlan in 100 200; do
  ip netns add "nlt-lan$vlan"
  ip -n nlt-switch link add link br0 name "nlt-v$vlan" type vlan id "$vlan"
  ip -n nlt-switch link set "nlt-v$vlan" netns "nlt-lan$vlan"
  ip -n "nlt-lan$vlan" link set "nlt-v$vlan" up
  ip -n "nlt-lan$vlan" link set lo up
  ip -n "nlt-lan$vlan" address add 192.0.2.1/24 dev "nlt-v$vlan"
  ip -n "nlt-lan$vlan" address add fd11::1/64 dev "nlt-v$vlan"
done

ip link add nlt-linux type bridge vlan_filtering 1
ip link set nlt-linux up
ip address add 192.0.2.254/24 dev nlt-linux
ip link add nlt-bport type veth peer name nlt-bpeer
ip link set nlt-bport master nlt-linux
bridge vlan add vid 100 dev nlt-bport
ip link set nlt-bport up
ip link set nlt-bpeer netns nlt-switch
ip -n nlt-switch link set nlt-bpeer master br0
ip -n nlt-switch link set nlt-bpeer up

ovs-vsctl add-br nlt-ovs
ip link set nlt-ovs up
ip address add 192.0.2.253/24 dev nlt-ovs
ip link add nlt-oport type veth peer name nlt-opeer
ip link set nlt-oport up
ovs-vsctl add-port nlt-ovs nlt-oport
ip link set nlt-opeer netns nlt-switch
ip -n nlt-switch link set nlt-opeer master br0
ip -n nlt-switch link set nlt-opeer up

for name in native 100 200; do
  namespace=nlt-switch
  if [[ $name != native ]]; then namespace="nlt-lan$name"; fi
  systemd-run --unit="nlt-lan-$name" --collect ip netns exec "$namespace" python3 -c '
import socket,sys
from http.server import ThreadingHTTPServer,BaseHTTPRequestHandler
class Handler(BaseHTTPRequestHandler):
 def do_GET(self):
  body=sys.argv[1].encode();self.send_response(200);self.send_header("Content-Length",str(len(body)));self.end_headers();self.wfile.write(body)
 def log_message(self,*args): pass
class Server(ThreadingHTTPServer): address_family=socket.AF_INET6
Server(("::",9000),Handler).serve_forever()
' "$name"
done
