from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/ci'))
from check_nix import host_targets, native_system
from policy import PolicyError


class NativeNixTests(unittest.TestCase):
    def test_only_native_host_closures_are_selected(self):
        hosts = {'x86-worker': 'x86_64-linux', 'arm-worker': 'aarch64-linux'}
        self.assertEqual(host_targets(hosts, 'aarch64-linux'), ['.#nixosConfigurations.arm-worker.config.system.build.toplevel'])

    def test_configuration_names_cannot_inject_nix_attribute_paths(self):
        with self.assertRaises(PolicyError):
            host_targets({'host.config.system': 'x86_64-linux'}, 'x86_64-linux')

    def test_non_linux_validation_is_rejected(self):
        with patch('check_nix.platform.system', return_value='Darwin'), patch('check_nix.platform.machine', return_value='aarch64'):
            with self.assertRaises(PolicyError):
                native_system()
