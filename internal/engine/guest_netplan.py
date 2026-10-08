import ipaddress
import json
from pathlib import Path
import subprocess
import sys

import yaml


def bind_interfaces(configurations, interfaces):
    addresses = {}
    for interface in interfaces:
        address, mac = interface['address'], interface['mac'].lower()
        if address in addresses and addresses[address] != mac:
            raise ValueError(f'Ambiguous interface address: {address}')
        addresses[address] = mac

    changes = []
    bindings = 0
    for path, configuration in configurations.items():
        for name, settings in configuration.get('network', {}).get('ethernets', {}).items():
            matched = {addresses[address] for value in settings.get('addresses', [])
                       if (address := str(ipaddress.ip_interface(value).ip)) in addresses}
            if len(matched) > 1:
                raise ValueError(f'Interface {name} matches multiple virtual NICs')
            if not matched:
                continue
            bindings += 1
            mac = matched.pop()
            binding = {'macaddress': mac}
            target_name = settings.get('set-name', name)
            if settings.get('match') != binding or settings.get('set-name') != target_name:
                changes.append((path, name, settings, binding, target_name))

    for _, _, settings, binding, target_name in changes:
        settings['match'], settings['set-name'] = binding, target_name
    return {path for path, *_ in changes}, bindings


def main():
    interfaces = json.loads(sys.argv[1])
    actual = {path.read_text().strip().lower() for path in Path('/sys/class/net').glob('*/address')}
    if any(interface['mac'].lower() not in actual for interface in interfaces):
        raise ValueError('Planned NIC is missing from the guest')
    configurations = {path: yaml.safe_load(path.read_text()) or {}
                      for path in sorted(Path('/etc/netplan').glob('*.yaml'))}
    changed, bindings = bind_interfaces(configurations, interfaces)
    for path in changed:
        path.write_text(yaml.safe_dump(configurations[path], sort_keys=False))
    # Apply on a repeated start too: a previous apply may have failed after writing.
    if bindings:
        result = subprocess.run(['netplan', 'apply'], capture_output=True, text=True)
        if result.returncode:
            sys.exit(result.stderr or f'netplan apply exited {result.returncode}')
    print(json.dumps({'changedFiles': len(changed)}))


if __name__ == '__main__':
    main()
