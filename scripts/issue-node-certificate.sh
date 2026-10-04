#!/usr/bin/env bash
set -euo pipefail
[[ $(id -u) == 0 ]]
address=${1:?用法：issue-node-certificate.sh 节点IP 输出目录}
directory=${2:?提供输出目录}
umask 077
mkdir -m 0700 -- "$directory"
openssl req -new -newkey rsa:3072 -nodes -subj '/CN=Netlab node' -keyout "$directory/node.key" -out "$directory/node.csr"
openssl x509 -req -in "$directory/node.csr" -CA /etc/netlab/pki/ca.crt -CAkey /etc/netlab/pki/ca.key -set_serial "0x$(openssl rand -hex 16)" -days 365 -extfile <(printf '%s\n' 'basicConstraints=critical,CA:FALSE' 'keyUsage=critical,digitalSignature,keyEncipherment' 'extendedKeyUsage=serverAuth,clientAuth' "subjectAltName=DNS:localhost,IP:127.0.0.1,IP:$address") -out "$directory/node.crt"
cp -- /etc/netlab/pki/ca.crt "$directory/ca.crt"
rm -- "$directory/node.csr"
