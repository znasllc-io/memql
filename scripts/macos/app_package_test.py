"""Exercise the shipped app installer in a temporary home, with no live agents."""
import argparse
import hashlib
import json
import os
import pathlib
import plistlib
import shutil
import subprocess
import tempfile
import tarfile
import io


# Stands in for MemQL.app's executable: `worker service` is recorded and
# answered without SMAppService, and everything else runs the real worker.
SERVICE_FIXTURE = '''#!/bin/bash
function main() {
    if [[ "$1" == worker && "$2" == service ]]; then
        printf '%s\\n' "$*" >> '@CALLS@'
        [[ "${MEMQL_TEST_SERVICE_FAIL:-}" != 1 ]] || return 1
        case "$3" in
            register)
                touch "$MEMQL_TEST_STATE/com.znasllc.memql-worker"
                [[ "$4" != --menu=on ]] || touch "$MEMQL_TEST_STATE/com.znasllc.memql-cockpit-menubar" ;;
            unregister) rm -f "$MEMQL_TEST_STATE/com.znasllc.memql-worker" "$MEMQL_TEST_STATE/com.znasllc.memql-cockpit-menubar" ;;
        esac
        echo '{"menu":"enabled","worker":"enabled"}'
        return 0
    fi
    exec '@REAL@' "$@"
}
main "$@"
'''


def run(command, env, ok=True):
    result = subprocess.run(command, env=env, text=True, capture_output=True)
    if ok and result.returncode:
        raise AssertionError(result.stdout + result.stderr)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--archive', required=True, type=pathlib.Path)
    archive = parser.parse_args().archive.resolve()
    repo = pathlib.Path(__file__).resolve().parents[2]
    with tempfile.TemporaryDirectory(prefix='memql-app-test-') as temp:
        root = pathlib.Path(temp)
        unpack = root / 'download'
        unpack.mkdir()
        env = dict(os.environ)
        # Exercise the same closed-layout and digest verification as install.
        run(['bash', '-c', 'source "$1"; fetch_macos_app "$2" "$3" "$4"', 'fixture',
             str(repo / 'scripts/install/lib.sh'), archive.parent.as_uri(),
             'arm64' if 'arm64' in archive.name else 'amd64', str(unpack)], env)
        source = unpack / 'unpacked'
        # A valid checksum does not authorize traversal or symlink extraction.
        for kind in ['traversal', 'symlink']:
            malicious = root / kind
            malicious.mkdir()
            bad_archive = malicious / archive.name
            with tarfile.open(bad_archive, 'w:gz') as tf:
                info = tarfile.TarInfo('MemQL.app/Contents/Resources/../escape')
                if kind == 'symlink':
                    info.name = 'MemQL.app/Contents/MacOS/MemQL'
                    info.type = tarfile.SYMTYPE
                    info.linkname = '/etc/passwd'
                else:
                    info.size = 1
                tf.addfile(info, io.BytesIO(b'x') if kind == 'traversal' else None)
            bad_archive.with_name(bad_archive.name + '.sha256').write_text(hashlib.sha256(bad_archive.read_bytes()).hexdigest() + '  ' + bad_archive.name + '\n')
            stage = malicious / 'stage'
            stage.mkdir()
            rejected = run(['bash', '-c', 'source "$1"; fetch_macos_app "$2" "$3" "$4"', 'fixture',
                            str(repo / 'scripts/install/lib.sh'), malicious.as_uri(),
                            'arm64' if 'arm64' in archive.name else 'amd64', str(stage)], env, ok=False)
            assert rejected.returncode == 3 and not (stage / 'unpacked').exists()
        # MemQL.app registers its own services through SMAppService
        # (memql-cockpit#545), and a test must never reach the real one: the
        # fixture app's executable records `worker service` and hands every
        # other command to the real worker, and the app is signed again.
        real_worker = root / 'real-memql'
        shutil.move(source / 'MemQL.app/Contents/MacOS/MemQL', real_worker)
        service_calls = root / 'service-calls'
        wrapper = source / 'MemQL.app/Contents/MacOS/MemQL'
        wrapper.write_text(SERVICE_FIXTURE.replace('@CALLS@', str(service_calls)).replace('@REAL@', str(real_worker)))
        wrapper.chmod(0o755)
        run(['codesign', '--force', '--sign', '-', str(source / 'MemQL.app')], env)
        user = root / 'user & space'
        user.mkdir()
        shim = root / 'shim'
        shim.mkdir()
        state = root / 'agents'
        state.mkdir()
        calls = root / 'calls'
        launch = shim / 'launchctl'
        launch.write_text('''#!/bin/bash
function main() {
    printf '%s\\n' "$*" >> "$MEMQL_TEST_CALLS"
    local label
    case "$1" in
        print)
            label="${2##*/}"
            if [[ -f "$MEMQL_TEST_STATE/$label.stopping" && "${MEMQL_TEST_STUBBORN:-}" != 1 ]]; then
                rm -f "$MEMQL_TEST_STATE/$label.stopping" "$MEMQL_TEST_STATE/$label"
                return 0 # one last visible observation before removal
            fi
            test -f "$MEMQL_TEST_STATE/$label" ;;
        bootstrap)
            label="$(/usr/libexec/PlistBuddy -c 'Print :Label' "$3")"
            [[ ! -f "$MEMQL_TEST_STATE/$label" ]] || return 5
            [[ "${MEMQL_TEST_BOOTSTRAP_FAIL:-}" != 1 ]] || return 5
            if [[ -f "$MEMQL_TEST_STATE/$label.retry" ]]; then
                rm "$MEMQL_TEST_STATE/$label.retry"; return 5
            fi
            touch "$MEMQL_TEST_STATE/$label" ;;
        bootout) label="${2##*/}"; touch "$MEMQL_TEST_STATE/$label.stopping" ;;
        unload) label="$(/usr/libexec/PlistBuddy -c 'Print :Label' "$2")"; rm -f "$MEMQL_TEST_STATE/$label" ;;
        kickstart) label="${@: -1}"; label="${label##*/}"; test -f "$MEMQL_TEST_STATE/$label" ;;
        *) return 98 ;;
    esac
}
main "$@"
''')
        launch.chmod(0o755)
        sleep = shim / 'sleep'
        sleep.write_text('#!/bin/bash\nexit 0\n')
        sleep.chmod(0o755)
        tcc = shim / 'tccutil'
        tcc.write_text('#!/bin/bash\nfunction main() { test "$#" = 3; }\nmain "$@"\n')
        tcc.chmod(0o755)

        # Some macOS versions print missing-key diagnostics to stdout. The
        # activation capability must still emit exactly one JSON result.
        plutil = shim / 'plutil'
        plutil.write_text('''#!/bin/bash
function main() {
    if [[ "$1" == -remove && "$2" == AssociatedBundleIdentifiers ]] && ! /usr/libexec/PlistBuddy -c 'Print :AssociatedBundleIdentifiers' "$3" >/dev/null 2>&1; then
        printf '%s\\n' 'No value to remove at key path AssociatedBundleIdentifiers'
        return 1
    fi
    /usr/bin/plutil "$@"
}
main "$@"
''')
        plutil.chmod(0o755)

        env.update(HOME=str(user), PATH=str(shim) + ':' + env['PATH'],
                   MEMQL_TEST_STATE=str(state), MEMQL_TEST_CALLS=str(calls))
        private = user / '.memql'
        (private / 'bin').mkdir(parents=True)
        (private / 'credentials').mkdir()
        protected = {}
        for name in ['worker.yaml', 'workers.yaml', 'policy.yaml', 'credentials/token']:
            path = private / name
            path.write_text('preserve-this-fixture-' + name)
            path.chmod(0o600)
            protected[path] = path.read_bytes()
        old_worker = private / 'bin/old-worker'
        old_worker.write_text('old rollback binary')
        cli = private / 'bin/memql'
        cli.symlink_to(old_worker)
        agents = user / 'Library/LaunchAgents'
        agents.mkdir(parents=True)
        worker_plist = agents / 'com.znasllc.memql-worker.plist'
        before = {'Label': 'com.znasllc.memql-worker', 'ProgramArguments': [str(cli), 'worker', 'run', '--log-level=debug'],
                  'RunAtLoad': True, 'KeepAlive': True, 'EnvironmentVariables': {'HOME': str(user), 'KEEP': 'yes'},
                  'StandardOutPath': str(private / 'custom.log')}
        worker_plist.write_bytes(plistlib.dumps(before))
        for label in ['com.znasllc.memql-cockpit-worker', 'com.visionarys.memql-cockpit-worker']:
            (agents / (label + '.plist')).write_bytes(plistlib.dumps({'Label': label, 'ProgramArguments': [str(old_worker)]}))
            (state / label).touch()
        legacy_menu = user / 'Applications/MemQL Cockpit.app'
        legacy_menu.parent.mkdir()
        shutil.copytree(source / 'MemQL.app/Contents/Library/LoginItems/MemQL Menu.app', legacy_menu)
        destination = user / 'Applications/MemQL.app'
        files = ['bash', str(source / 'scripts/macos/install-app-files.sh'), '--app=' + str(source / 'MemQL.app'),
                 '--destination=' + str(destination), '--cli-path=' + str(cli)]
        result = json.loads(run(files, env).stdout)
        assert result['changed'] is True
        assert cli.resolve() == (destination / 'Contents/MacOS/MemQL').resolve()
        assert old_worker.read_text() == 'old rollback binary'
        backups = list(destination.parent.glob('.memql-app-rollback.*'))
        assert any((p / 'previous-cli').is_symlink() for p in backups)
        assert json.loads(run(files, env).stdout)['changed'] is False
        assert list(destination.parent.glob('.memql-app-rollback.*')) == backups
        info = plistlib.loads((destination / 'Contents/Info.plist').read_bytes())
        assert info['CFBundleIdentifier'] == 'com.znasllc.memql-worker'
        assert info['CFBundleExecutable'] == info['CFBundleDisplayName'] == info['CFBundleName'] == 'MemQL'
        assert (destination / 'Contents/Resources' / info['CFBundleIconFile']).stat().st_size > 1000
        # The earlier install's worker is loaded from its per-user plist.
        (state / 'com.znasllc.memql-worker').touch()
        activate = ['bash', str(source / 'scripts/macos/activate-app.sh'), '--app=' + str(destination)]
        assert json.loads(run(activate + ['--ollama-env=OLLAMA_MODELS=/Volumes/Models'], env).stdout)['changed'] is True
        # Its plists leave, each kept under backups, and the app registers
        # the worker and the menu itself.
        assert not worker_plist.exists() and not (agents / 'com.znasllc.memql-cockpit-menubar.plist').exists()
        kept = list((private / 'backups').glob('legacy-agent.*/com.znasllc.memql-worker.plist'))
        assert len(kept) == 1 and plistlib.loads(kept[0].read_bytes()) == before
        for label in ['com.znasllc.memql-cockpit-worker', 'com.visionarys.memql-cockpit-worker']:
            assert not (agents / (label + '.plist')).exists()
            assert not (state / label).exists()
        # A new app registers afresh: launchd refuses an executable whose
        # signature no longer matches the one recorded at registration.
        assert service_calls.read_text().splitlines() == ['worker service unregister', 'worker service register --menu=on']
        assert (private / 'state/service.env').read_text() == 'OLLAMA_MODELS=/Volumes/Models\n'
        assert not legacy_menu.exists()
        assert list(user.glob('Applications/.memql-legacy-menu.*/previous.app'))
        assert json.loads(run(activate, env).stdout)['changed'] is False
        assert service_calls.read_text().splitlines()[2:] == ['worker service register --menu=on'], 'idempotent activation released a service'
        assert (private / 'state/service.env').read_text() == 'OLLAMA_MODELS=/Volumes/Models\n', 'no Ollama variables keeps the saved ones'
        run(activate + ['--ollama-env=OLLAMA_HOST=127.0.0.1:11500', '--ollama-env=OLLAMA_MODELS=/Volumes/Other'], env)
        assert (private / 'state/service.env').read_text() == 'OLLAMA_HOST=127.0.0.1:11500\nOLLAMA_MODELS=/Volumes/Other\n'
        assert run(activate + ['--ollama-env=DYLD_INSERT_LIBRARIES=/tmp/x.dylib'], env, ok=False).returncode == 2
        assert all(path.read_bytes() == data for path, data in protected.items())
        run(activate + ['--menu=false'], env)
        assert service_calls.read_text().splitlines()[-1] == 'worker service register --menu=off'
        refused = run(activate, dict(env, MEMQL_TEST_SERVICE_FAIL='1'), ok=False)
        assert refused.returncode == 5 and not json.loads(refused.stdout)['ok']
        # A changed app whose services could not be released is not marked
        # current, so the next activation tries again.
        marker = private / 'state/app-activation.sha256'
        marker.write_text('old build')
        assert run(activate, dict(env, MEMQL_TEST_SERVICE_FAIL='1'), ok=False).returncode == 5 and not marker.exists()
        assert json.loads(run(activate, env).stdout)['changed'] is True
        assert service_calls.read_text().splitlines()[-2:] == ['worker service unregister', 'worker service register --menu=on']
        # A loaded per-user job that will not stop keeps its plist, and
        # nothing is registered over it.
        worker_plist.write_bytes(plistlib.dumps(before))
        (state / 'com.znasllc.memql-worker').touch()
        registered = len(service_calls.read_text().splitlines())
        stubborn = run(activate, dict(env, MEMQL_TEST_STUBBORN='1'), ok=False)
        assert stubborn.returncode == 5 and worker_plist.exists()
        assert len(service_calls.read_text().splitlines()) == registered
        assert json.loads(run(activate, env).stdout)['ok'] and not worker_plist.exists()
        # A valid source does not authorize replacing an unrelated app.
        bad = root / 'unrelated/MemQL.app'
        (bad / 'Contents').mkdir(parents=True)
        (bad / 'Contents/Info.plist').write_bytes(plistlib.dumps({'CFBundleIdentifier': 'other.app'}))
        refused = run(files[:-2] + ['--destination=' + str(bad), '--cli-path=' + str(cli)], env, ok=False)
        assert refused.returncode == 3
        # Full uninstall is tested only in this disposable fixture home.
        run(['bash', str(repo / 'scripts/install/uninstall-mac.sh'), '--user-local', '--all-homes'], env)
        assert service_calls.read_text().splitlines()[-1] == 'worker service unregister', 'only the app takes its services out of Login Items'
        assert not destination.exists() and not cli.exists()
        assert not (private / 'worker.yaml').exists() and not (private / 'workers.yaml').exists()
        assert (private / 'policy.yaml').read_bytes() == protected[private / 'policy.yaml']
        assert (private / 'credentials/token').read_bytes() == protected[private / 'credentials/token']
        assert backups[0].exists()
        print('PASS: app identity/icon, archive traversal/link refusal, CLI rollback, app-registered services, retired per-user plists, service.env, idempotence, preserved config/credentials, legacy cleanup, scoped uninstall; service calls stubbed.')


if __name__ == '__main__':
    main()
