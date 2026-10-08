#!/usr/bin/env bash

# Called after jq and util-linux are installed. Existing mounted storage is a directory.
select_storage() {
  local requested_data=${1:-/var/lib/netlab-node} requested_device=${2:-auto}
  local candidate
  candidate=$(lsblk -J -b -p -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS,RO |
    jq -r '[.blockdevices[] | select(.type=="disk" and .ro==false and .fstype==null and ((.children // [])|length)==0 and ([.mountpoints[]? | select(.!=null)]|length)==0)] | sort_by(.size) | last | .name // ""')
  data=$requested_data
  storage_device=$requested_device
  if [[ $requested_device == auto ]]; then storage_device=$candidate; fi
  if [[ -t 0 ]]; then
    read -r -p "本地数据目录 [$data]：" requested_data
    data=${requested_data:-$data}
    read -r -p "共享存储盘 [${storage_device:-none}]（交由 Ceph 使用；none 保留本地存储）：" requested_device
    storage_device=${requested_device:-$storage_device}
  elif [[ $requested_device == auto && -n $candidate ]]; then
    echo '非交互安装请明确指定共享存储盘，或填写 none。' >&2
    exit 1
  fi
  [[ $data == /* && $data != / ]] || { echo '数据目录应为独立绝对路径。' >&2; exit 1; }
  [[ $storage_device != none ]] || storage_device=''
  if [[ -n $storage_device ]]; then
    storage_device=$(readlink -f -- "$storage_device")
    [[ -b $storage_device ]] || { echo '共享存储位置不是块设备。' >&2; exit 1; }
    lsblk -J -p -o NAME,TYPE,FSTYPE,MOUNTPOINTS,RO "$storage_device" |
      jq -e '.blockdevices | length==1 and (.[0] | .type=="disk" and .ro==false and .fstype==null and ((.children // [])|length)==0 and ([.mountpoints[]? | select(.!=null)]|length)==0)' >/dev/null ||
      { echo '共享存储盘已有分区、文件系统或正在使用。' >&2; exit 1; }
    wipefs -n -J "$storage_device" | jq -e '(.signatures // []) | length==0' >/dev/null ||
      { echo '共享存储盘已有数据签名。' >&2; exit 1; }
    for stable_device in /dev/disk/by-id/*; do
      if [[ $(readlink -f -- "$stable_device") == "$storage_device" && $stable_device != *-part* ]]; then storage_device=$stable_device; break; fi
    done
    apt-get install -y cephadm podman openssh-server lvm2 chrony
    systemctl enable --now ssh chrony
  fi
  install -d -m 0711 -- "$data"
}
