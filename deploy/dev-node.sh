#!/usr/bin/env bash
set -euo pipefail
umask 077

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
owner=$(stat -c %u "$repo")
user_directory=$(getent passwd "$owner" | cut -d: -f6)
pki="$user_directory/.local/share/netlab-dev/pki"
data=/var/lib/netlab-dev
node_address=$(ip -4 route get 1.1.1.1 | awk '{for (i=1;i<=NF;i++) if ($i=="src") print $(i+1)}')
export PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

install -d -m 700 -o "$owner" -g "$owner" "$pki"
install -d -m 711 "$data" "$data/bin" "$data/templates"

if [[ ! -f "$pki/ca.crt" ]]; then
  openssl req -x509 -newkey rsa:3072 -nodes -days 365 -subj '/CN=Netlab local development CA' \
    -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -keyout "$pki/ca.key" -out "$pki/ca.crt"
fi
for role in controller node; do
  if [[ $role == controller && -f "$pki/controller.crt" ]]; then
    continue
  fi
  if [[ -f "$pki/$role.key" ]]; then
    openssl req -new -subj "/CN=Netlab development $role" -key "$pki/$role.key" -out "$pki/$role.csr"
  else
    openssl req -new -newkey rsa:3072 -nodes -subj "/CN=Netlab development $role" \
      -keyout "$pki/$role.key" -out "$pki/$role.csr"
  fi
  if [[ $role == node ]]; then
    extensions=$'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:localhost,IP:127.0.0.1,IP:'"$node_address"
  else
    extensions=$'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth'
  fi
  openssl x509 -req -in "$pki/$role.csr" -CA "$pki/ca.crt" -CAkey "$pki/ca.key" \
    -set_serial "0x$(openssl rand -hex 16)" -days 365 -extfile <(printf '%s\n' "$extensions") \
    -out "$pki/$role.crt"
  rm "$pki/$role.csr"
done
chown "$owner:$owner" "$pki"/*
chmod 600 "$pki"/*.key
chmod 644 "$pki"/*.crt

if [[ ! -f "$data/templates/base.qcow2" ]]; then
  qemu-img create -f qcow2 "$data/templates/base.qcow2" 1G
  chmod 644 "$data/templates/base.qcow2"
fi

cd "$repo"
go build -mod=readonly -tags libvirt_dlopen -o "$data/bin/netlab-node" ./cmd/node
if [[ $(systemctl show -p LoadState --value netlab-node-dev.service) != not-found ]]; then
  systemctl stop netlab-node-dev.service
fi
systemd-run --unit=netlab-node-dev --collect --property=Restart=on-failure \
  "$data/bin/netlab-node" --data "$data" --listen :19443 \
  --ca "$pki/ca.crt" --cert "$pki/node.crt" --key "$pki/node.key"

printf 'Node endpoint: https://%s:19443\nCA: %s/ca.crt\nController certificate: %s/controller.crt\nController key: %s/controller.key\nVM artifact: %s/templates/base.qcow2\n' "$node_address" "$pki" "$pki" "$pki" "$data"
