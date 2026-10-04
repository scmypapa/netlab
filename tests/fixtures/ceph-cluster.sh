#!/usr/bin/env bash
set -euo pipefail

# Dedicated test cluster: its only OSD is a newly created D-drive backing file.
root=/var/lib/netlab-dev/ceph-test
backing=/mnt/d/.cache/netlab/ceph-test-osd.img
address=192.168.122.1
mkdir -m 0700 "$root"
config=$root/ceph.conf
fsid=$(uuidgen)
cat >"$config" <<CONFIG
[global]
fsid = $fsid
mon host = [v2:$address:13300,v1:$address:16789]
auth cluster required = cephx
auth service required = cephx
auth client required = cephx
auth allow insecure global id reclaim = false
public network = 192.168.122.0/24
cluster network = 192.168.122.0/24
osd pool default size = 1
osd pool default min size = 1
mon allow pool size one = true
osd memory target = 536870912
osd crush chooseleaf type = 0
log to file = false
log to stderr = true
CONFIG
ceph-authtool --create-keyring "$root/mon.keyring" --gen-key -n mon. --cap mon 'allow *' >/dev/null
ceph-authtool --create-keyring "$root/admin.keyring" --gen-key -n client.admin --cap mon 'allow *' --cap osd 'allow *' --cap mgr 'allow *' --cap mds 'allow *' >/dev/null
ceph-authtool "$root/mon.keyring" --import-keyring "$root/admin.keyring" >/dev/null
monmaptool --create --fsid "$fsid" --addv test "[v2:$address:13300,v1:$address:16789]" "$root/monmap" >/dev/null
mkdir "$root/mon" "$root/mgr" "$root/osd"
ceph-mon -c "$config" -i test --mkfs --mon-data "$root/mon" --monmap "$root/monmap" --keyring "$root/mon.keyring" --setuser root --setgroup root >"$root/setup.log" 2>&1
systemd-run --unit=netlab-ceph-test-mon --service-type=simple ceph-mon -f -c "$config" -i test --mon-data "$root/mon" --setuser root --setgroup root
ceph=(ceph -c "$config" --keyring "$root/admin.keyring" --name client.admin)
"${ceph[@]}" --connect-timeout 30 status
"${ceph[@]}" auth get-or-create mgr.test mon 'allow profile mgr' osd 'allow *' mds 'allow *' -o "$root/mgr/keyring"
systemd-run --unit=netlab-ceph-test-mgr --service-type=simple ceph-mgr -f -c "$config" -i test --mgr-data "$root/mgr" --keyring "$root/mgr/keyring" --setuser root --setgroup root
[[ ! -e "$backing" ]]
truncate -s 32G "$backing"
device=$(losetup --find --show "$backing")
printf '%s\n' "$device" >"$root/device"
osd_uuid=$(uuidgen)
osd_id=$("${ceph[@]}" osd new "$osd_uuid")
printf '%s\n' "$osd_id" >"$root/osd-id"
ceph-authtool --create-keyring "$root/osd/keyring" --gen-key -n "osd.$osd_id" >/dev/null
"${ceph[@]}" auth add "osd.$osd_id" osd 'allow *' mon 'allow profile osd' mgr 'allow profile osd' -i "$root/osd/keyring"
ceph-osd -c "$config" -i "$osd_id" --mkfs --osd-uuid "$osd_uuid" --osd-data "$root/osd" --osd-objectstore bluestore --bluestore-block-path "$device" --keyring "$root/osd/keyring" --setuser root --setgroup root >>"$root/setup.log" 2>&1
"${ceph[@]}" osd crush add "osd.$osd_id" 0.01 host=netlab-test root=default
systemd-run --unit=netlab-ceph-test-osd --service-type=simple ceph-osd -f -c "$config" -i "$osd_id" --osd-data "$root/osd" --keyring "$root/osd/keyring" --setuser root --setgroup root
"${ceph[@]}" osd pool create netlab 8 8
"${ceph[@]}" osd pool set netlab size 1 --yes-i-really-mean-it
"${ceph[@]}" osd pool set netlab pg_autoscale_mode off
rbd -c "$config" --keyring "$root/admin.keyring" --id admin pool init netlab
"${ceph[@]}" auth get-or-create client.netlab mon 'profile rbd' osd 'profile rbd pool=netlab' mgr 'profile rbd pool=netlab' -o "$root/client.keyring"
chmod 0600 "$root"/*.keyring "$root"/*/keyring
"${ceph[@]}" status
