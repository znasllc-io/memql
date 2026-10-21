"""Exercise ownership and failure cleanup before this fixture may touch Docker."""
import importlib.util
import json
from pathlib import Path
import subprocess
import unittest

spec = importlib.util.spec_from_file_location('fleet_fixture', Path(__file__).with_name('fleet-fixture.py'))
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class RecordedFixture(fixture.Fixture):
    def __init__(self):
        self.nonce, self.daemon = 'owned', 'daemon-one'
        self.name, self.volume, self.image = 'container-name', 'volume-name', 'image-name'
        self.container = 'a' * 64
        self.calls, self.changed, self.foreign, self.replaced, self.copy_failed = [], False, False, False, False

    def call(self, *args, **kwargs):
        self.calls.append(args)
        if args[0] == 'info':
            return 'daemon-two' if self.changed else self.daemon
        if len(args) > 1 and args[1] == 'inspect':
            labels = {fixture.LABEL: 'foreign' if self.foreign else self.nonce}
            return json.dumps([{'Id': 'b' * 64 if self.replaced else 'a' * 64,
                                'Labels': labels, 'Config': {'Labels': labels}}])
        if args[0] == 'cp' and self.copy_failed:
            raise subprocess.CalledProcessError(1, args, stderr='copy failed')
        return ''


class FixtureTests(unittest.TestCase):
    def test_nested_daemon_has_finite_resources_and_only_its_own_volume(self):
        args = fixture.create_args('one', 'image', 'owned-volume', 'a' * 40, 'repair', 'nonce')
        for limit in ('--cpus=4', '--memory=16g', '--memory-swap=16g', '--pids-limit=8192', '--cgroupns=private'):
            self.assertIn(limit, args)
        self.assertEqual([args[i + 1] for i, arg in enumerate(args) if arg == '--volume'],
                         ['owned-volume:/var/lib/docker'])
        self.assertNotIn('--network', args)
        self.assertNotIn('--pid', args)
        self.assertNotIn('--mount', args)
        self.assertNotIn('/var/run/docker.sock', ' '.join(args))
        self.assertEqual([args[i + 1].split('=')[0] for i, arg in enumerate(args) if arg == '--env'],
                         ['MEMQL_SHA', 'MEMQL_INSTALL_LEG'])

    def test_cleanup_retains_evidence_then_removes_only_owned_objects(self):
        item = RecordedFixture()
        item.cleanup(Path('/evidence'))
        effects = [call for call in item.calls if call[0] not in ('info', 'container') and call[1] != 'inspect']
        self.assertEqual(effects, [('stop', '--time=30', 'a' * 64),
                                  ('cp', 'a' * 64 + ':/evidence/.', '/evidence'),
                                  ('rm', '--force', '--volumes', 'a' * 64),
                                  ('volume', 'rm', 'volume-name'), ('image', 'rm', 'image-name')])

    def test_fast_case_uses_a_smaller_finite_ceiling(self):
        args = fixture.create_args('one', 'image', 'owned-volume', 'a' * 40, 'no-cluster', 'nonce')
        self.assertIn('--cpus=2', args)
        self.assertIn('--memory=4g', args)
        self.assertIn('--memory-swap=4g', args)

    def test_failed_evidence_copy_is_reported_but_does_not_leave_resources(self):
        item = RecordedFixture()
        item.copy_failed = True
        with self.assertRaisesRegex(RuntimeError, 'cleanup incomplete'):
            item.cleanup(Path('/evidence'))
        self.assertIn(('rm', '--force', '--volumes', 'a' * 64), item.calls)
        self.assertIn(('volume', 'rm', 'volume-name'), item.calls)
        self.assertIn(('image', 'rm', 'image-name'), item.calls)

    def test_reaper_recovers_the_exact_owned_container_before_stopping_it(self):
        item = RecordedFixture()
        item.container = None
        item.cleanup(Path('/evidence'))
        self.assertEqual(item.container, 'a' * 64)
        self.assertIn(('stop', '--time=30', 'a' * 64), item.calls)

    def test_changed_daemon_is_refused_before_any_mutation(self):
        item = RecordedFixture()
        item.changed = True
        with self.assertRaisesRegex(RuntimeError, 'identity changed'):
            item.cleanup(Path('/evidence'))
        self.assertTrue(all(call[0] == 'info' for call in item.calls))

    def test_foreign_or_replaced_container_is_never_stopped(self):
        for field in ('foreign', 'replaced'):
            with self.subTest(field=field):
                item = RecordedFixture()
                setattr(item, field, True)
                with self.assertRaises(RuntimeError):
                    item.cleanup(Path('/evidence'))
                self.assertFalse(any(call[0] in ('stop', 'rm') for call in item.calls))

    def test_permission_failure_cannot_be_reported_as_already_clean(self):
        item = RecordedFixture()
        def fail(*args, **kwargs):
            if args[0] == 'info':
                return item.daemon
            raise subprocess.CalledProcessError(1, args, stderr='permission denied')
        item.call = fail
        with self.assertRaises(subprocess.CalledProcessError):
            item.cleanup(Path('/evidence'))


if __name__ == '__main__':
    unittest.main()
