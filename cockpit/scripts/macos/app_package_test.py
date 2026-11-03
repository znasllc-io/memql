"""Exercise the shipped app installer in a temporary home, with no live agents."""
import argparse
import hashlib
import json
import os
import pathlib
import plistlib
import shutil
import subprocess
import sys
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
                case "${MEMQL_TEST_REGISTER:-}" in
                    once)
                        if [[ ! -f "$MEMQL_TEST_STATE/register-retried" ]]; then
                            touch "$MEMQL_TEST_STATE/register-retried"
                            echo 'ERROR: Operation not permitted (ServiceManagement error 1)' >&2
                            return 1
                        fi ;;
                    denied|approval|unreadable)
                        echo 'ERROR: Operation not permitted (ServiceManagement error 1)' >&2
                        return 1 ;;
                    signature)
                        echo 'ERROR: Invalid signature (ServiceManagement error 2)' >&2
                        return 1 ;;
                    # What macOS 27 does for a build without a Team ID
                    # registered over an earlier record: success, and a job
                    # that never runs.
                    dead) return 0 ;;
                esac
                touch "$MEMQL_TEST_STATE/com.znasllc.memql-worker"
                [[ "$4" != --menu=on ]] || touch "$MEMQL_TEST_STATE/com.znasllc.memql-cockpit-menubar" ;;
            unregister) rm -f "$MEMQL_TEST_STATE/com.znasllc.memql-worker" "$MEMQL_TEST_STATE/com.znasllc.memql-cockpit-menubar" ;;
            status)
                if [[ "${MEMQL_TEST_REGISTER:-}" == unreadable ]]; then
                    echo 'status unavailable'
                    return 0
                fi
                if [[ "${MEMQL_TEST_REGISTER:-}" == approval ]]; then
                    echo '{"menu":"not_registered","worker":"requires_approval"}'
                    return 0
                fi ;;
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
    run([sys.executable, str(repo / 'scripts/macos/app_service_native_test.py')], dict(os.environ))
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
        # A MemQL.app with a Team ID registers its own services through
        # SMAppService (memql-cockpit#545), and a test must never reach the
        # real one: the fixture app's executable records `worker service` and
        # hands every other command to the real worker, and the app is signed
        # again. launchctl is a shim too; no test reaches launchd.
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
# A loaded job is a file named for its label, holding the plist it was
# bootstrapped from; one the app registered (or the test planted) holds none.
function main() {
    printf '%s\\n' "$*" >> "$MEMQL_TEST_CALLS"
    local label from program respawns
    case "$1" in
        print)
            label="${2##*/}"
            if [[ -f "$MEMQL_TEST_STATE/$label.stopping" && "${MEMQL_TEST_STUBBORN:-}" != 1 ]]; then
                rm -f "$MEMQL_TEST_STATE/$label.stopping" "$MEMQL_TEST_STATE/$label"
                return 0 # one last visible observation before removal
            fi
            test -f "$MEMQL_TEST_STATE/$label" || return 113
            from="$(cat "$MEMQL_TEST_STATE/$label")"
            program="$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$from" 2>/dev/null || true)"
            printf '%s = {\\n\\tpath = %s\\n' "$2" "$from"
            if [[ -n "$program" && "$program" == "${MEMQL_TEST_SPAWN_FAILED_PROGRAM:-}" ]]; then
                printf '\\tstate = spawn failed\\n'
            elif [[ -n "$program" && "$program" == "${MEMQL_TEST_RESPAWNING_PROGRAM:-}" ]]; then
                # Exits at once and is started again: running, never twice
                # as the same process.
                respawns="$(cat "$MEMQL_TEST_STATE/respawns" 2>/dev/null || echo 0)"
                printf '%s' "$((respawns + 1))" > "$MEMQL_TEST_STATE/respawns"
                printf '\\tstate = running\\n\\tpid = %s\\n' "$((5000 + respawns))"
            else
                printf '\\tstate = running\\n\\tpid = 4242\\n'
            fi
            printf '}\\n' ;;
        print-disabled)
            [[ -z "${MEMQL_TEST_DISABLED:-}" ]] || printf 'disabled services = {\\n\\t"%s" => disabled\\n}\\n' "$MEMQL_TEST_DISABLED" ;;
        bootstrap)
            label="$(/usr/libexec/PlistBuddy -c 'Print :Label' "$3")"
            [[ ! -f "$MEMQL_TEST_STATE/$label" ]] || return 5
            [[ "${MEMQL_TEST_BOOTSTRAP_FAIL:-}" != 1 ]] || return 5
            program="$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$3" 2>/dev/null || true)"
            [[ -z "$program" || "$program" != "${MEMQL_TEST_REFUSED_PROGRAM:-}" ]] || return 5
            if [[ -f "$MEMQL_TEST_STATE/$label.retry" ]]; then
                rm "$MEMQL_TEST_STATE/$label.retry"; return 5
            fi
            printf '%s' "$3" > "$MEMQL_TEST_STATE/$label" ;;
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
        # The fixture is signed ad-hoc, like every release: no Team ID. A test
        # that needs the SMAppService path names one.
        codesign = shim / 'codesign'
        codesign.write_text('''#!/bin/bash
function main() {
    if [[ "$1" == --display && -n "${MEMQL_TEST_TEAM_ID:-}" ]]; then
        printf 'TeamIdentifier=%s\\n' "$MEMQL_TEST_TEAM_ID" >&2
        return 0
    fi
    exec /usr/bin/codesign "$@"
}
main "$@"
''')
        codesign.chmod(0o755)

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
        (state / 'com.znasllc.memql-worker').write_text(str(worker_plist))
        activate = ['bash', str(source / 'scripts/macos/activate-app.sh'), '--app=' + str(destination)]
        executable = str(destination / 'Contents/MacOS/MemQL')
        helper = destination / 'Contents/Library/LoginItems/MemQL Menu.app'
        menu_plist = agents / 'com.znasllc.memql-cockpit-menubar.plist'
        marker = private / 'state/app-activation.sha256'
        service_path = ('/usr/bin:/bin:/usr/sbin:/sbin:' + str(user) + '/.local/bin:' + str(user) + '/.claude/local:'
                        '/opt/homebrew/bin:/usr/local/bin')

        def launched(start):
            return [line for line in calls.read_text().splitlines()[start:] if line.startswith(('bootout ', 'bootstrap '))]

        # A build without a Team ID -- every release -- runs from per-user
        # plists (the owner's decision, 2026-11-03). The earlier plist keeps
        # everything but its executable, and nothing asks SMAppService.
        first = json.loads(run(activate + ['--ollama-env=OLLAMA_MODELS=/Volumes/Models'], env).stdout)
        assert first['changed'] is True and first['result']['service'] == 'launch_agent'
        written = plistlib.loads(worker_plist.read_bytes())
        assert written['ProgramArguments'] == [executable, 'worker', 'run', '--log-level=debug']
        assert written['StandardOutPath'] == str(private / 'custom.log') and written['KeepAlive'] is True
        assert written['EnvironmentVariables'] == {'HOME': str(user), 'KEEP': 'yes', 'PATH': service_path, 'MEMQL_SERVICE': 'app'}
        assert written['AssociatedBundleIdentifiers'] == ['com.znasllc.memql-worker']
        kept = [pathlib.Path(p) for p in first['result']['previous_service_files'] if p.endswith('/com.znasllc.memql-worker.plist')]
        assert len(kept) == 1 and plistlib.loads(kept[0].read_bytes()) == before
        assert plistlib.loads(menu_plist.read_bytes())['ProgramArguments'] == [str(helper / 'Contents/MacOS/MemQLCockpit')]
        assert (state / 'com.znasllc.memql-worker').read_text() == str(worker_plist)
        assert (state / 'com.znasllc.memql-cockpit-menubar').read_text() == str(menu_plist)
        for label in ['com.znasllc.memql-cockpit-worker', 'com.visionarys.memql-cockpit-worker']:
            assert not (agents / (label + '.plist')).exists()
            assert not (state / label).exists()
        assert not service_calls.exists(), 'a build without a Team ID asked SMAppService'
        assert (private / 'state/service.env').read_text() == 'OLLAMA_MODELS=/Volumes/Models\n'
        assert not legacy_menu.exists()
        assert list(user.glob('Applications/.memql-legacy-menu.*/previous.app'))
        start = len(calls.read_text().splitlines())
        assert json.loads(run(activate, env).stdout)['changed'] is False
        assert launched(start) == [], 'an unchanged activation restarted a service'
        assert (private / 'state/service.env').read_text() == 'OLLAMA_MODELS=/Volumes/Models\n', 'no Ollama variables keeps the saved ones'
        run(activate + ['--ollama-env=OLLAMA_HOST=127.0.0.1:11500', '--ollama-env=OLLAMA_MODELS=/Volumes/Other'], env)
        assert (private / 'state/service.env').read_text() == 'OLLAMA_HOST=127.0.0.1:11500\nOLLAMA_MODELS=/Volumes/Other\n'
        assert run(activate + ['--ollama-env=DYLD_INSERT_LIBRARIES=/tmp/x.dylib'], env, ok=False).returncode == 2
        assert all(path.read_bytes() == data for path, data in protected.items())
        assert json.loads(run(activate + ['--menu=false'], env).stdout)['changed'] is True
        assert not menu_plist.exists() and not (state / 'com.znasllc.memql-cockpit-menubar').exists()
        assert (state / 'com.znasllc.memql-worker').exists()
        assert json.loads(run(activate, env).stdout)['changed'] is True and menu_plist.exists()
        # An earlier release's SMAppService registration holds the label from
        # no plist. It cannot run a new build, so it is replaced, not reused.
        (state / 'com.znasllc.memql-worker').write_text('')
        start = len(calls.read_text().splitlines())
        assert json.loads(run(activate, env).stdout)['changed'] is True
        assert 'bootout gui/' + str(os.getuid()) + '/com.znasllc.memql-worker' in launched(start)
        assert (state / 'com.znasllc.memql-worker').read_text() == str(worker_plist)
        # Switched off in Login Items: refused before anything stops, because
        # turning it back on is the person's call. A switched-off menu warns.
        start = len(calls.read_text().splitlines())
        switched_off = run(activate, dict(env, MEMQL_TEST_DISABLED='com.znasllc.memql-worker'), ok=False)
        assert switched_off.returncode == 3 and 'Login Items' in json.loads(switched_off.stdout)['error']['message']
        assert launched(start) == [] and (state / 'com.znasllc.memql-worker').exists()
        quiet = run(activate, dict(env, MEMQL_TEST_DISABLED='com.znasllc.memql-cockpit-menubar'))
        assert json.loads(quiet.stdout)['ok'] and 'switched off' in quiet.stderr
        # A worker that does not start from a new definition -- bootstrap
        # refused, loaded and never running, or running only between
        # respawns -- gets the previous one back, running, and the failure
        # says so. Nothing is marked current.
        for variable in ['MEMQL_TEST_REFUSED_PROGRAM', 'MEMQL_TEST_SPAWN_FAILED_PROGRAM', 'MEMQL_TEST_RESPAWNING_PROGRAM']:
            worker_plist.write_bytes(plistlib.dumps(before))
            (state / 'com.znasllc.memql-worker').write_text(str(worker_plist))
            failed = run(activate, dict(env, **{variable: executable}), ok=False)
            response = json.loads(failed.stdout)
            assert failed.returncode == 5 and 'previous one is back and running' in response['error']['message'], failed.stdout + failed.stderr
            assert plistlib.loads(worker_plist.read_bytes()) == before
            assert (state / 'com.znasllc.memql-worker').read_text() == str(worker_plist)
            assert any(plistlib.loads(pathlib.Path(p).read_bytes()) == before for p in response['result']['previous_service_files'])
            assert not marker.exists()
        assert json.loads(run(activate, env).stdout)['ok'] and marker.exists()
        # A per-user job that will not stop keeps its plist and keeps running.
        worker_plist.write_bytes(plistlib.dumps(before))
        stubborn = run(activate, dict(env, MEMQL_TEST_STUBBORN='1'), ok=False)
        assert stubborn.returncode == 5 and plistlib.loads(worker_plist.read_bytes()) == before
        assert (state / 'com.znasllc.memql-worker').exists()
        assert json.loads(run(activate, env).stdout)['ok']

        # A build with a Team ID registers through SMAppService
        # (memql-cockpit#545). The per-user plists stop and are set aside, the
        # worker's last, and a changed app registers afresh.
        team = dict(env, MEMQL_TEST_TEAM_ID='ABCDE12345')
        registered = json.loads(run(activate, team).stdout)
        assert registered['changed'] is True and registered['result']['service'] == 'app'
        assert not worker_plist.exists() and not menu_plist.exists()
        assert service_calls.read_text().splitlines() == ['worker service unregister', 'worker service register --menu=on']
        assert [pathlib.Path(p).name for p in registered['result']['previous_service_files']] == [
            'com.znasllc.memql-cockpit-menubar.plist', 'com.znasllc.memql-worker.plist']
        assert json.loads(run(activate, team).stdout)['changed'] is False
        assert service_calls.read_text().splitlines()[2:] == ['worker service register --menu=on'], 'idempotent activation released a service'
        run(activate + ['--menu=false'], team)
        assert service_calls.read_text().splitlines()[-1] == 'worker service register --menu=off'
        # A refusal, or a registered worker that never runs, cannot leave the
        # Mac without a worker: the plists set aside come back and this build
        # starts from them before the failure is reported (owner, 2026-11-03).
        for failure, attempts, exit_code in [('denied', 3, 5), ('approval', 1, 3), ('signature', 1, 5), ('unreadable', 1, 5), ('dead', 1, 5)]:
            marker.write_text('previous build')
            worker_plist.write_bytes(plistlib.dumps(before))
            (state / 'com.znasllc.memql-worker').write_text(str(worker_plist))
            start = len(service_calls.read_text().splitlines())
            refused = run(activate, dict(team, MEMQL_TEST_REGISTER=failure), ok=False)
            response = json.loads(refused.stdout)
            assert refused.returncode == exit_code and not response['ok'], refused.stdout + refused.stderr
            assert response['result']['service'] == 'launch_agent'
            observed = service_calls.read_text().splitlines()[start:]
            assert observed.count('worker service unregister') == 1
            assert observed.count('worker service register --menu=on') == attempts
            assert observed.count('worker service status') == (0 if failure == 'dead' else attempts)
            assert plistlib.loads(worker_plist.read_bytes())['ProgramArguments'][0] == executable
            assert (state / 'com.znasllc.memql-worker').read_text() == str(worker_plist), 'a refusal left no worker running'
            assert any(plistlib.loads(pathlib.Path(p).read_bytes()) == before for p in response['result']['previous_service_files'])
            assert all(path.read_bytes() == data for path, data in protected.items())
        # So is macOS refusing to release the previous registration.
        marker.write_text('old build')
        refused = run(activate, dict(team, MEMQL_TEST_SERVICE_FAIL='1'), ok=False)
        assert refused.returncode == 5 and (state / 'com.znasllc.memql-worker').read_text() == str(worker_plist)
        # Apple's unregister completion can still race registration. Retry
        # only register, only for this transition, and preserve any real refusal.
        marker.write_text('previous build')
        start = len(service_calls.read_text().splitlines())
        retried = run(activate, dict(team, MEMQL_TEST_REGISTER='once'))
        assert json.loads(retried.stdout)['ok'] and marker.read_text().rstrip().endswith('service=app')
        assert service_calls.read_text().splitlines()[start:] == [
            'worker service unregister', 'worker service register --menu=on',
            'worker service status', 'worker service register --menu=on']
        # An unchanged activation must not turn ordinary denied registration
        # into a retry loop, because it performed no completed unregister.
        start = len(service_calls.read_text().splitlines())
        refused = run(activate, dict(team, MEMQL_TEST_REGISTER='denied'), ok=False)
        assert refused.returncode == 5
        assert service_calls.read_text().splitlines()[start:] == [
            'worker service register --menu=on', 'worker service status']
        # A valid source does not authorize replacing an unrelated app.
        bad = root / 'unrelated/MemQL.app'
        (bad / 'Contents').mkdir(parents=True)
        (bad / 'Contents/Info.plist').write_bytes(plistlib.dumps({'CFBundleIdentifier': 'other.app'}))
        refused = run(files[:-2] + ['--destination=' + str(bad), '--cli-path=' + str(cli)], env, ok=False)
        assert refused.returncode == 3
        # Full uninstall is tested only in this disposable fixture home. A
        # build without a Team ID registered nothing, so nothing is
        # unregistered: its per-user plists stop by label and go.
        asked = len(service_calls.read_text().splitlines())
        run(['bash', str(repo / 'scripts/install/uninstall-mac.sh'), '--user-local', '--all-homes'], env)
        assert len(service_calls.read_text().splitlines()) == asked, 'an app without a Team ID was asked to unregister'
        assert not worker_plist.exists() and not menu_plist.exists()
        assert not destination.exists() and not cli.exists()
        assert not (private / 'worker.yaml').exists() and not (private / 'workers.yaml').exists()
        assert (private / 'policy.yaml').read_bytes() == protected[private / 'policy.yaml']
        assert (private / 'credentials/token').read_bytes() == protected[private / 'credentials/token']
        assert backups[0].exists()
        print('PASS: app identity/icon, archive traversal/link refusal, CLI rollback, per-user services without a Team ID '
              '(preserved plist, replaced registration, switched-off refusal, previous definition restored), app-registered '
              'services with one (per-user fallback on every refusal), service.env, idempotence, preserved config/credentials, '
              'legacy cleanup, scoped uninstall; service calls stubbed.')

if __name__ == '__main__':
    main()
