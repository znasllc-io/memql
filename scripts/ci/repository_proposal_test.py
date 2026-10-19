import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('proposal', Path(__file__).with_name('repository-proposal.py'))
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)
SHA = 'a' * 40


class Proposals(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    @patch.dict(os.environ, {'MEMQL_REPOSITORY': 'acme/project', 'MEMQL_GITHUB_READ_TOKEN': 'scoped-reader'})
    def test_reader_credential_stays_on_its_exact_repository(self):
        with patch.object(p.urllib.request, 'build_opener') as opener:
            opener.return_value.open.return_value.__enter__.return_value.read.return_value = b'{}'
            for endpoint, expected in [('/repos/acme/project/pulls', 'Bearer scoped-reader'),
                                       ('/repos/acme/project-other/pulls', None),
                                       ('/repos/znasllc-io/memql/releases/latest', None)]:
                p.github(endpoint)
                request = opener.return_value.open.call_args.args[0]
                self.assertEqual(request.get_header('Authorization'), expected)

    def release(self, tag='v0.30.0', exists=False):
        return lambda path, **_: ({'draft': False, 'prerelease': False, 'tag_name': tag}
                                 if path.endswith('/latest') else ({} if exists else None))

    def test_watch_changes_only_assignment_and_keeps_newer_pins(self):
        original = '# old version v0.29.32 stays in this comment\nENGINE_REF=v0.29.32\nPRODUCT=example\n'
        (self.root / 'product.env').write_text(original)
        result = p.engine_watch(self.root, 'acme/product', SHA, self.release())
        self.assertEqual(result['files'][0]['content'], original.replace('ENGINE_REF=v0.29.32', 'ENGINE_REF=v0.30.0'))
        self.assertEqual(len(result['files']), 1)
        self.assertEqual(p.engine_watch(self.root, 'acme/product', SHA, self.release('v0.29.31'))['files'], [])
        self.assertEqual(p.engine_watch(self.root, 'acme/product', SHA, self.release(exists=True))['files'], [])

    def test_watch_never_executes_environment_file_and_refuses_ambiguous_pins(self):
        for text in ['ENGINE_REF=$(touch escaped)\n', 'ENGINE_REF=v0.29.32\nENGINE_REF=v0.29.33\n']:
            (self.root / 'product.env').write_text(text)
            with self.assertRaises(ValueError):
                p.engine_watch(self.root, 'acme/product', SHA, self.release())
        self.assertFalse((self.root / 'escaped').exists())

    def test_watch_unstamped_no_network_and_symlink_refused(self):
        def unexpected(*_args, **_kwargs):
            self.fail('unstamped template read network')
        self.assertEqual(p.engine_watch(self.root, 'acme/product', SHA, unexpected)['files'], [])
        (self.root / 'target').write_text('ENGINE_REF=v0.29.32\n')
        (self.root / 'product.env').symlink_to('target')
        with self.assertRaises(ValueError):
            p.engine_watch(self.root, 'acme/product', SHA, self.release())

    def test_convergence_compares_contents_not_unchanged_status_names(self):
        file = self.root / 'go.mod'
        file.write_text('initial')
        calls = []
        def run(command, **kwargs):
            self.assertEqual(kwargs['env']['GOTOOLCHAIN'], 'local')
            self.assertNotIn('MEMQL_GITHUB_READ_TOKEN', kwargs['env'])
            if command == ['go', 'mod', 'tidy']:
                calls.append(command)
                file.write_text('second' if len(calls) == 1 else 'third')
        result = p.converge(self.root, ['go.mod'], ['go.mod'], run)
        self.assertEqual(len(calls), 3)
        self.assertEqual(result['go.mod'], b'third')

    def init_git(self):
        subprocess.run(['git', 'init', '-q', str(self.root)], check=True)
        (self.root / 'go.mod').write_text('module example.test/project\n\ngo 1.26.1\n')
        (self.root / 'go.work').write_text('go 1.26.1\n\nuse .\n')
        (self.root / 'source.go').write_text('package project\n')
        subprocess.run(['git', '-C', str(self.root), 'add', 'go.mod', 'go.work', 'source.go'], check=True)
        subprocess.run(['git', '-C', str(self.root), '-c', 'user.name=fixture', '-c', 'user.email=fixture@example.test', 'commit', '-qm', 'fixture'], check=True)

    def metadata(self, changed='go.mod', author='dependabot[bot]'):
        def read(path, **_kwargs):
            if '/commits/' in path:
                return [{'number': 7, 'state': 'open', 'user': {'login': author}, 'head': {
                    'sha': SHA, 'repo': {'full_name': 'acme/project'}, 'ref': 'dependabot/go_modules/bump'}}]
            return [{'filename': changed}]
        return read

    @patch.dict(os.environ, {'MEMQL_EVENT': 'pull_request'})
    def test_tidy_refuses_code_changes_before_running_toolchain(self):
        self.init_git()
        def forbidden(*_args):
            self.fail('ran toolchain for a code change')
        with self.assertRaises(ValueError):
            p.dependabot_tidy(self.root, 'acme/project', SHA, self.metadata('source.go'), forbidden)
        self.assertEqual(p.dependabot_tidy(self.root, 'acme/project', SHA, self.metadata(author='human'), forbidden)['files'], [])

    @patch.dict(os.environ, {'MEMQL_EVENT': 'pull_request'})
    def test_tidy_proposal_keeps_base_hash_and_new_sum_absence(self):
        self.init_git()
        def tidy(root, _modules, paths):
            (root / 'go.sum').write_text('example.test/module v1.0.0 h1:fixture\n')
            return {name: p.regular_bytes(root, name) for name in paths}
        proposal = p.dependabot_tidy(self.root, 'acme/project', SHA, self.metadata(), tidy)
        self.assertEqual([f['path'] for f in proposal['files']], ['go.sum'])
        self.assertEqual(proposal['files'][0]['baseSha256'], '')

    def test_real_go_tidy_is_a_noop_for_a_clean_offline_workspace(self):
        self.init_git()
        modules, paths = p.manifest_paths(self.root)
        before = {name: p.regular_bytes(self.root, name) for name in paths}
        with patch.dict(os.environ, {'GOPROXY': 'off', 'GOSUMDB': 'off'}):
            self.assertEqual(p.converge(self.root, modules, paths), before)


if __name__ == '__main__':
    unittest.main()
