"""Validate the exact piped local installer without touching a real home/service."""
import argparse
import contextlib
import functools
import hashlib
import http.server
import shutil
import shlex
import threading
import json
import os
import pathlib
import plistlib
import subprocess
import tempfile
import urllib.request


@contextlib.contextmanager
def local_source(worker, archive, version):
    """CI serves exactly the same standalone scripts/assets as local Fleet."""
    repo = pathlib.Path(__file__).resolve().parents[2]
    with tempfile.TemporaryDirectory(prefix='memql-http-source-') as temp:
        root = pathlib.Path(temp)
        scripts = root / 'scripts/install'
        scripts.mkdir(parents=True)
        for name in ['install-mac.sh', 'uninstall-mac.sh', 'lib.sh']:
            shutil.copy2(repo / 'scripts/install' / name, scripts / name)
        assets = root / 'releases/download' / ('v' + version)
        assets.mkdir(parents=True)
        for source in [worker, archive, archive.with_name(archive.name + '.sha256')]:
            shutil.copy2(source, assets / source.name)
        # The installers verify the worker against its release checksum
        # before running it (memql-cockpit#525), so the local release
        # carries one too, in the release's own spelling: the computer-use
        # sidecar scripts/ci/release-checksum.py writes, or memql-SHA256SUMS.
        manifest = worker.name + '.sha256' if worker.name.startswith('memql-computeruse-') else 'memql-SHA256SUMS'
        digest = hashlib.sha256(worker.read_bytes()).hexdigest()
        (assets / manifest).write_text(digest + '  ' + worker.name + '\n')
        class QuietHandler(http.server.SimpleHTTPRequestHandler):
            def log_message(self, *_args):
                pass
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(QuietHandler, directory=str(root)))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield 'http://127.0.0.1:' + str(server.server_port)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


def stop_fixture_process(process):
    if process.poll() is None:
        process.terminate()
    try:
        process.wait(timeout=3)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=3)


def exercise(base, version):
    base = base.rstrip('/')
    with urllib.request.urlopen(base + '/scripts/install/install-mac.sh') as response:
        script = response.read()
    with urllib.request.urlopen(base + '/scripts/install/uninstall-mac.sh') as response:
        uninstaller = response.read()
    with tempfile.TemporaryDirectory(prefix='memql-full-install-') as temp, contextlib.ExitStack() as processes:
        root = pathlib.Path(temp)
        user = root / 'fresh user & space'
        user.mkdir()
        shim = root / 'shim'
        shim.mkdir()
        # --no-service must avoid even attempting a launchctl invocation.
        launch = shim / 'launchctl'
        launch.write_text('#!/bin/bash\nfunction main() { echo forbidden-live-service-call >&2; exit 99; }\nmain "$@"\n')
        launch.chmod(0o755)
        env = {k: v for k, v in os.environ.items() if not k.startswith('MEMQL_INSTALL_')}
        env.update(HOME=str(user), PATH=str(shim) + ':' + env['PATH'],
                   MEMQL_INSTALL_RAW_BASE=base + '/scripts/install',
                   MEMQL_INSTALL_VERSION=version, MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP='1')
        with urllib.request.urlopen(base + '/scripts/install/lib.sh') as response:
            library = root / 'lib.sh'
            library.write_bytes(response.read())
        # HTTP requires explicit opt-in, a literal loopback host, and no redirect.
        for url, allow in [(base + '/manifest.json', ''), (base.replace('127.0.0.1', 'localhost') + '/scripts/install/lib.sh', '1'), (base + '/scripts/install', '1')]:
            rejected_env = dict(env, MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP=allow)
            output = root / 'refused-download'
            rejected = subprocess.run(['bash', '-c', 'source "$1"; download_binary "$2" "$3"', 'fixture', str(library), url, str(output)], env=rejected_env, capture_output=True)
            assert rejected.returncode != 0 and not output.exists() and not output.with_suffix('.partial').exists()
        privacy_calls = root / 'privacy-calls'
        env['MEMQL_TEST_PRIVACY_CALLS'] = str(privacy_calls)
        tcc = shim / 'tccutil'
        tcc.write_text('''#!/bin/bash
function main() {
    [[ $# == 3 && "$1" == reset ]] || return 99
    case "$2" in Accessibility|ScreenCapture) ;; *) return 99 ;; esac
    case "$3" in com.znasllc.memql-worker|com.znasllc.memql-cockpit-menubar) ;; *) return 99 ;; esac
    printf '%s\\n' "$*" >> "$MEMQL_TEST_PRIVACY_CALLS"
    [[ "${MEMQL_TEST_TCC_FAIL:-}" != 1 || "$2" != ScreenCapture ]] || return 1
}
main "$@"
''')
        tcc.chmod(0o755)
        seed = user / '.memql/bin/memql'
        seed.parent.mkdir(parents=True)
        # Same release/prerelease, different commit: an installer that skips on
        # the release number will later reject the matching native app bundle.
        previous = version + '+gfixtureold'
        seed.write_text('#!/bin/bash\nfunction main() { echo ' + shlex.quote('memql ' + previous + ' (computeruse)') + '; }\nmain "$@"\n')
        seed.chmod(0o755)
        token = 'mql_wkr_fixture_only_not_a_real_enrollment'
        command = ['bash', '-s', '--', '--verbose', '--token', token, '--cluster', 'https://fixture.invalid',
                   '--name', 'fresh-install-fixture', '--computeruse', '--user-local', '--no-service', '--no-menu',
                   '--download-base=' + base + '/releases/download/v' + version]
        result = subprocess.run(command, input=script, env=env, cwd=root, capture_output=True)
        if result.returncode:
            raise AssertionError(result.stdout.decode() + result.stderr.decode())
        app = user / 'Applications/MemQL.app'
        info = plistlib.loads((app / 'Contents/Info.plist').read_bytes())
        assert info['CFBundleShortVersionString'] == version
        assert info['CFBundleIdentifier'] == 'com.znasllc.memql-worker'
        cli = user / '.memql/bin/memql'
        assert cli.resolve() == (app / 'Contents/MacOS/MemQL').resolve()
        registry = (user / '.memql/workers.yaml').read_text()
        assert token in registry and 'https://fixture.invalid' in registry
        assert not (user / 'Library/LaunchAgents').exists()
        assert b'forbidden-live-service-call' not in (result.stdout + result.stderr)
        assert token.encode() not in result.stdout and token.encode() not in result.stderr
        # The same command can enroll again without changing the installed app.
        before = (app / 'Contents/MacOS/MemQL').read_bytes()
        repeated = subprocess.run(command, input=script, env=env, cwd=root, capture_output=True)
        assert repeated.returncode == 0, repeated.stdout.decode() + repeated.stderr.decode()
        assert (app / 'Contents/MacOS/MemQL').read_bytes() == before
        assert b'signing requirement unchanged' in repeated.stdout
        assert not privacy_calls.exists(), 'unchanged update must preserve permissions'
        # The installer writes the legacy mirror's state_dir per home
        # (<root>/homes/<id>); a --purge plan must name the root, never
        # the per-home directory, and change nothing. --no-service left
        # no state directory behind, so give the plan one to judge.
        (user / '.memql/state').mkdir(exist_ok=True)
        (user / '.memql/state/worker.log').write_text('log line')
        plan = subprocess.run(['bash', '-s', '--', '--verbose', '--user-local', '--purge', '--dry-run'], input=uninstaller, env=env, cwd=root, capture_output=True)
        assert plan.returncode == 0, plan.stdout.decode() + plan.stderr.decode()
        assert ('would remove:  ' + str(user / '.memql/state') + ' (recursively)').encode() in plan.stdout
        assert b'homes/fixture.invalid' not in plan.stdout
        assert token in (user / '.memql/workers.yaml').read_text() and (user / '.memql/worker.yaml').exists()
        info_path = app / 'Contents/Info.plist'
        prior_info = plistlib.loads(info_path.read_bytes())
        prior_info['CFBundleVersion'] = 'prior-local-build'
        info_path.write_bytes(plistlib.dumps(prior_info))
        subprocess.run(['codesign', '--force', '--sign', '-', str(app)], check=True, capture_output=True)
        # A partially failed reset must stop before replacing the installed app.
        failed_upgrade = subprocess.run(command, input=script,
            env=dict(env, MEMQL_TEST_TCC_FAIL='1'), cwd=root, capture_output=True)
        assert failed_upgrade.returncode == 5, failed_upgrade.stdout + failed_upgrade.stderr
        assert b'could not clear obsolete MemQL permissions' in (failed_upgrade.stdout + failed_upgrade.stderr)
        assert plistlib.loads(info_path.read_bytes())['CFBundleVersion'] == 'prior-local-build'
        assert token in (user / '.memql/workers.yaml').read_text()
        privacy_calls.unlink()
        upgraded = subprocess.run(command, input=script, env=env, cwd=root, capture_output=True)
        assert upgraded.returncode == 0, upgraded.stdout.decode() + upgraded.stderr.decode()
        assert b'signing identity changed' in upgraded.stdout
        assert b'Obsolete MemQL approvals cleared' in upgraded.stdout
        assert privacy_calls.read_text().splitlines() == [
            'reset Accessibility com.znasllc.memql-worker',
            'reset ScreenCapture com.znasllc.memql-worker']
        privacy_calls.unlink()
        alternate = root / 'other-install/MemQL.app'
        (alternate / 'Contents').mkdir(parents=True)
        (alternate / 'Contents/Info.plist').write_bytes(plistlib.dumps({'CFBundleIdentifier': 'com.znasllc.memql-worker'}))
        scope = subprocess.run(['bash', '-c', 'source "$1"; macos_privacy_scope_unique "$2"', 'fixture', str(library), str(alternate)], env=env, capture_output=True)
        assert scope.returncode == 3 and b'no reset performed' in scope.stderr
        assert not privacy_calls.exists()

        private = user / '.memql'
        protected = {}
        for name in ['policy.yaml', 'state/worker.log', 'credentials/token', 'clusters.yaml', 'certs/client.pem', 'backups/previous-worker']:
            path = private / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('keep fixture ' + name)
            protected[path] = path.read_bytes()
        second = command.copy()
        second[second.index(token)] = 'mql_wkr_other_fixture'
        second[second.index('https://fixture.invalid')] = 'https://other.invalid'
        paired = subprocess.run(second, input=script, env=env, cwd=root, capture_output=True)
        assert paired.returncode == 0, paired.stdout.decode() + paired.stderr.decode()
        # MemQL.app takes its own services out of Login Items on uninstall
        # (memql-cockpit#545). The fixture app's executable records that and
        # runs every other command on the real worker, so no test reaches
        # SMAppService.
        real_worker = root / 'real-memql'
        shutil.copyfile(app / 'Contents/MacOS/MemQL', real_worker)
        real_worker.chmod(0o755)
        service_calls = root / 'app-service-calls'
        (app / 'Contents/MacOS/MemQL').write_text('#!/bin/bash\nfunction main() {\n'
            '    if [[ "$1" == worker && "$2" == service ]]; then\n'
            '        printf \'%s\\n\' "$*" >> ' + shlex.quote(str(service_calls)) + '\n'
            '        [[ "$3" != register || -z "${MEMQL_TEST_STATE:-}" ]] || touch "$MEMQL_TEST_STATE/com.znasllc.memql-worker"\n'
            '        return 0\n'
            '    fi\n'
            '    exec ' + shlex.quote(str(real_worker)) + ' "$@"\n}\nmain "$@"\n')

        def uninstall(*flags, expected=0):
            result = subprocess.run(['bash', '-s', '--', '--verbose', '--user-local', *flags], input=uninstaller, env=env, cwd=root, capture_output=True)
            assert result.returncode == expected, result.stdout.decode() + result.stderr.decode()
            for secret in [token.encode(), b'mql_wkr_other_fixture']:
                assert secret not in result.stdout and secret not in result.stderr
            return result

        saved_registry = (private / 'workers.yaml').read_bytes()
        uninstall('--cluster=https://fixture.invalid', '--purge', expected=3)
        assert (private / 'workers.yaml').read_bytes() == saved_registry
        # Two enrollments and no --cluster/--all-homes: the run must say which
        # one, so it refuses (2) naming both and the command for each.
        ambiguous = uninstall(expected=2)
        assert b'--cluster=https://fixture.invalid' in (ambiguous.stdout + ambiguous.stderr) and b'--cluster=https://other.invalid' in (ambiguous.stdout + ambiguous.stderr)
        assert (private / 'workers.yaml').read_bytes() == saved_registry
        # A --cluster value the binary would reject is refused as a bad
        # parameter (2) before anything runs; so is a space form whose URL
        # was omitted, which must not swallow the flag after it.
        for bad in (['--cluster=fixture.invalid'], ['--cluster=https://fixture.invalid?x=1'], ['--cluster', '--dry-run'], ['--cluster', '']):
            refused = uninstall(*bad, expected=2)
            assert b'--cluster' in refused.stderr
        assert (private / 'workers.yaml').read_bytes() == saved_registry
        (private / 'workers.yaml').write_text('homes: [invalid yaml')
        uninstall('--cluster=https://fixture.invalid', expected=5)
        assert (private / 'workers.yaml').read_text() == 'homes: [invalid yaml'
        (private / 'workers.yaml').write_bytes(saved_registry)
        # Simulated loaded agents exercise stop/reload, without launchd calls.
        agents = user / 'Library/LaunchAgents'
        agents.mkdir(parents=True)
        worker_label = 'com.znasllc.memql-worker'
        menu_label = 'com.znasllc.memql-cockpit-menubar'
        state = root / 'agent-state'
        state.mkdir()
        for label in [worker_label, menu_label]:
            (state / label).touch()
            (agents / (label + '.plist')).write_bytes(plistlib.dumps({'Label': label, 'ProgramArguments': [str(cli), 'worker', 'run']}))
        # Two harmless native wait processes: only the one at the exact
        # fixture app's embedded path is owned by this uninstall. No AppKit
        # application is launched and no live user process is targeted. Build
        # a tiny executable instead of relocating an Apple platform binary.
        sleeper = root / 'wait-fixture'
        subprocess.run(['xcrun', 'clang', '-x', 'c', '-o', str(sleeper), '-'],
                       input=b'#include <unistd.h>\nint main(void) { for (;;) pause(); }\n', check=True, capture_output=True)
        helper = app / 'Contents/Library/LoginItems/MemQL Menu.app/Contents/MacOS/MemQLCockpit'
        shutil.copyfile(sleeper, helper)
        helper.chmod(0o755)
        menu_process = subprocess.Popen([str(helper), '120'])
        processes.callback(stop_fixture_process, menu_process)
        unrelated = root / 'unrelated/MemQLCockpit'
        unrelated.parent.mkdir()
        shutil.copyfile(sleeper, unrelated)
        unrelated.chmod(0o755)
        unrelated_process = subprocess.Popen([str(unrelated), '120'])
        processes.callback(stop_fixture_process, unrelated_process)
        env.update(MEMQL_TEST_STATE=str(state), MEMQL_TEST_CALLS=str(root / 'service-calls'))
        launch.write_text('''#!/bin/bash
function main() {
    local label pending remaining
    printf '%s\\n' "$*" >> "$MEMQL_TEST_CALLS"
    case "$1" in
        print)
            label="${2##*/}"
            pending="$MEMQL_TEST_STATE/$label.pending"
            if [[ -f "$pending" ]]; then
                remaining="$(cat "$pending")"
                if [[ "$remaining" -gt 0 ]]; then
                    printf '%s\\n' "$((remaining - 1))" > "$pending"
                    return 0
                fi
                rm -f "$pending" "$MEMQL_TEST_STATE/$label"
            fi
            test -f "$MEMQL_TEST_STATE/$label" ;;
        bootout)
            [[ "${MEMQL_TEST_FAIL_STOP:-}" != 1 ]] || return 5
            [[ "${MEMQL_TEST_STUBBORN_STOP:-}" != 1 ]] || return 0
            if [[ "${MEMQL_TEST_DELAY_STOP:-}" == 1 ]]; then
                printf '3\\n' > "$MEMQL_TEST_STATE/${2##*/}.pending"
            else
                rm -f "$MEMQL_TEST_STATE/${2##*/}"
            fi ;;
        bootstrap)
            label="$(/usr/libexec/PlistBuddy -c 'Print :Label' "$3")"
            touch "$MEMQL_TEST_STATE/$label" ;;
        *) return 98 ;;
    esac
}
main "$@"
''')
        # A worker MemQL.app registered has no plist to reload: the app
        # starts it again for the enrollment that remains.
        held = root / 'held-worker.plist'
        (agents / (worker_label + '.plist')).rename(held)
        env['MEMQL_TEST_DELAY_STOP'] = '1'
        uninstall('--cluster=https://fixture.invalid')
        del env['MEMQL_TEST_DELAY_STOP']
        assert service_calls.read_text().splitlines() == ['worker service register']
        held.rename(agents / (worker_label + '.plist'))
        service_calls.unlink()
        calls = (root / 'service-calls').read_text().splitlines()
        stop_index = next(i for i, line in enumerate(calls) if line.startswith('bootout '))
        assert all(line.startswith('print ') for line in calls[stop_index + 1:stop_index + 5]), 'delayed removal was not rechecked'
        assert cli.exists() and app.exists()
        assert (state / worker_label).exists() and (state / menu_label).exists()
        assert menu_process.poll() is None and unrelated_process.poll() is None
        assert not privacy_calls.exists(), 'sibling uninstall reset shared permissions'
        registry = (private / 'workers.yaml').read_text()
        mirror = (private / 'worker.yaml').read_text()
        assert token not in registry and token not in mirror
        assert 'mql_wkr_other_fixture' in registry and 'mql_wkr_other_fixture' in mirror
        assert all(path.read_bytes() == value for path, value in protected.items())
        # A URL no enrollment matches is refused (3) naming the enrolled one;
        # a wrong URL never removes someone else's enrollment.
        mismatch = uninstall('--cluster=https://fixture.invalid', expected=3)
        assert b'--cluster=https://other.invalid' in (mismatch.stdout + mismatch.stderr)
        assert 'mql_wkr_other_fixture' in (private / 'workers.yaml').read_text()
        env['MEMQL_TEST_FAIL_STOP'] = '1'
        uninstall('--cluster=https://other.invalid', expected=5)
        assert cli.exists() and app.exists() and 'mql_wkr_other_fixture' in (private / 'workers.yaml').read_text()
        del env['MEMQL_TEST_FAIL_STOP']
        env['MEMQL_TEST_STUBBORN_STOP'] = '1'
        stubborn = uninstall('--cluster=https://other.invalid', expected=5)
        assert b'still loaded after 10s' in (stubborn.stdout + stubborn.stderr)
        assert cli.exists() and app.exists() and (agents / (worker_label + '.plist')).exists()
        assert 'mql_wkr_other_fixture' in (private / 'workers.yaml').read_text()
        del env['MEMQL_TEST_STUBBORN_STOP']
        assert not privacy_calls.exists(), 'failed stop reset permissions'
        env['MEMQL_TEST_TCC_FAIL'] = '1'
        failed_privacy = uninstall('--cluster=https://other.invalid', expected=5)
        assert b'permission cleanup did not fully succeed' in (failed_privacy.stdout + failed_privacy.stderr)
        assert app.exists() and cli.exists(), 'reset failure deleted the resolvable app'
        del env['MEMQL_TEST_TCC_FAIL']
        env['MEMQL_TEST_DELAY_STOP'] = '1'
        uninstall('--cluster=https://other.invalid')
        del env['MEMQL_TEST_DELAY_STOP']
        assert not app.exists() and not cli.is_symlink()
        assert set(service_calls.read_text().splitlines()) == {'worker service unregister'}, 'only a whole removal unregisters the app services'
        assert menu_process.wait(timeout=3) != 0, 'embedded helper was not terminated'
        assert unrelated_process.poll() is None, 'unrelated same-name process was stopped'
        assert not list(agents.glob('*.plist')) and not list(state.iterdir())
        assert not (private / 'worker.yaml').exists() and not (private / 'workers.yaml').exists()
        expected_resets = {'reset ' + service + ' ' + bundle for service in ['Accessibility', 'ScreenCapture'] for bundle in ['com.znasllc.memql-worker', 'com.znasllc.memql-cockpit-menubar']}
        assert set(privacy_calls.read_text().splitlines()) == expected_resets
        assert len(privacy_calls.read_text().splitlines()) == 8
        assert all(path.read_bytes() == value for path, value in protected.items())
        # No enrollment at all: --cluster=URL has nothing to unpair and
        # proceeds as --all-homes, so a repeated run is idempotent.
        uninstall('--cluster=https://other.invalid')
        # Missing runtime cannot safely parse remaining homes: preserve and refuse.
        (private / 'workers.yaml').write_bytes(saved_registry)
        uninstall('--cluster=https://fixture.invalid', expected=4)
        assert (private / 'workers.yaml').read_bytes() == saved_registry
        uninstall('--all-homes', '--purge')
        assert not (private / 'policy.yaml').exists() and not (private / 'state').exists()
        for path, value in protected.items():
            if path.name != 'policy.yaml' and path.parent.name != 'state':
                assert path.read_bytes() == value
        # A custom purge target cannot sweep backups or escape through aliases.
        for custom_state in [private, private / 'backups']:
            (private / 'worker.yaml').write_text('state_dir: ' + str(custom_state) + '\n')
            uninstall('--all-homes', '--purge')
            assert (private / 'backups/previous-worker').read_bytes() == protected[private / 'backups/previous-worker']
        outside = root / 'outside'
        (outside / 'state').mkdir(parents=True)
        (outside / 'state/keep').write_text('untouched')
        (private / 'alias').symlink_to(outside, target_is_directory=True)
        (private / 'worker.yaml').write_text('state_dir: ' + str(private / 'alias/state') + '\n')
        uninstall('--all-homes', '--purge')
        assert (outside / 'state/keep').read_text() == 'untouched'
        print(json.dumps({'ok': True, 'version': version, 'tested': 'piped HTTP fresh/repeated install; multi-home scoped uninstall and mirror repair; shared purge refusal; delayed bootout and stubborn/failed stop; malformed/missing runtime safety; last-home and repeated cleanup; explicit purge; credentials/backups retained; exact-path embedded helper cleanup with sibling/unrelated process retention; scoped permission resets and failure retention; shared bundle-ID refusal; unchanged/changed signing identity updates; no OS services or registration; no token output'}))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--base-url')
    parser.add_argument('--worker', type=pathlib.Path)
    parser.add_argument('--archive', type=pathlib.Path)
    parser.add_argument('--version', required=True)
    args = parser.parse_args()
    if args.base_url:
        exercise(args.base_url, args.version)
    else:
        if not args.worker or not args.archive:
            parser.error('supply --base-url or both --worker and --archive')
        with local_source(args.worker.resolve(), args.archive.resolve(), args.version) as base:
            exercise(base, args.version)


if __name__ == '__main__':
    main()
