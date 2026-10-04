#!/bin/bash
set -e
if [ "$2" = install ]; then
  mkdir -p /home/ubuntu/rdp-packages
  tar -xzf /home/ubuntu/rdp-packages.tar.gz -C /home/ubuntu/rdp-packages
  sudo -n env DEBIAN_FRONTEND=noninteractive dpkg -i /home/ubuntu/rdp-packages/*.deb
  rm /home/ubuntu/rdp-packages.tar.gz
  rm -rf /home/ubuntu/rdp-packages
fi
chmod 600 /home/ubuntu/.netlab-rdp-password
password=$(cat /home/ubuntu/.netlab-rdp-password)
printf 'ubuntu:%s\n' "$password" | sudo -n chpasswd
rm /home/ubuntu/.netlab-rdp-password
printf 'PasswordAuthentication yes\n' | sudo -n tee /etc/ssh/sshd_config.d/20-netlab-test.conf >/dev/null
sudo -n systemctl restart ssh
sudo -n usermod -aG ssl-cert xrdp
sudo -n make-ssl-cert generate-default-snakeoil --force-overwrite
rm -f /home/ubuntu/.netlab-desktop-ready /home/ubuntu/rdp-proof.txt
printf '. /home/ubuntu/.bashrc\ntouch /home/ubuntu/.netlab-desktop-ready\n' > /home/ubuntu/.netlab-desktop-bashrc
printf 'xterm -fa Monospace -fs 13 -geometry 100x30 -T Netlab-%s -e bash --rcfile /home/ubuntu/.netlab-desktop-bashrc &\nexec openbox\n' "$1" > /home/ubuntu/.xsession
sudo -n systemctl restart xrdp-sesman xrdp
rm /home/ubuntu/netlab-rdp-setup.sh
