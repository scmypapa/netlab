import copy
import unittest

from guest_netplan import bind_interfaces


class NetworkBindingTest(unittest.TestCase):
    def test_binding_preserves_network_roles_when_nic_order_changes(self):
        original = {'network': {'version': 2, 'ethernets': {
            'ens18': {'addresses': ['10.66.0.15/24'], 'gateway4': '10.66.0.1',
                      'nameservers': {'addresses': ['10.66.0.2']}},
            'ens19': {'addresses': ['172.22.1.15/24', 'fd00::15/64'],
                      'routes': [{'to': '172.23.0.0/16', 'via': '172.22.1.1'}]},
        }, 'bridges': {'br0': {'interfaces': ['ens19']}}}}
        configs = {'original.yaml': copy.deepcopy(original)}
        interfaces = [
            {'address': '172.22.1.15', 'mac': '02:00:00:00:00:02'},
            {'address': '10.66.0.15', 'mac': '02:00:00:00:00:01'},
        ]
        self.assertEqual(bind_interfaces(configs, interfaces), ({'original.yaml'}, 2))
        for name, mac in [('ens18', interfaces[1]['mac']), ('ens19', interfaces[0]['mac'])]:
            settings = configs['original.yaml']['network']['ethernets'][name]
            self.assertEqual(settings.pop('match'), {'macaddress': mac})
            self.assertEqual(settings.pop('set-name'), name)
        self.assertEqual(configs['original.yaml'], original)

    def test_ambiguous_mapping_changes_no_configuration(self):
        configs = {'config.yaml': {'network': {'ethernets': {
            'first': {'addresses': ['10.0.0.1/24']},
            'ambiguous': {'addresses': ['10.0.0.1/24', '10.0.0.2/24']},
        }}}}
        original = copy.deepcopy(configs)
        with self.assertRaisesRegex(ValueError, 'multiple virtual NICs'):
            bind_interfaces(configs, [{'address': '10.0.0.1', 'mac': 'a'},
                                     {'address': '10.0.0.2', 'mac': 'b'}])
        self.assertEqual(configs, original)

    def test_already_bound_configuration_is_not_rewritten(self):
        configs = {'config.yaml': {'network': {'ethernets': {
            'business': {'addresses': ['10.0.0.1/24'], 'match': {'macaddress': 'a'}, 'set-name': 'lan0'},
            'dhcp': {'dhcp4': True},
        }}}}
        self.assertEqual(bind_interfaces(configs, [{'address': '10.0.0.1', 'mac': 'a'}]), (set(), 1))


if __name__ == '__main__':
    unittest.main()
