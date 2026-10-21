#!/usr/bin/env python3
"""Run one installer case on an opted-in Linux fleet machine's local Docker.

The privileged fixture is trusted repository test code, not a shared Workbench.
Its daemon, OS, trust store, HOME and k3d cluster are disposable. No host socket,
directory, network namespace or credentials are passed into it.
"""
import argparse
import json
import os
from pathlib import Path
import re
import select
import shutil
import signal
import subprocess
import sys
import tempfile
import uuid

LABEL = 'io.memql.installer-proof'
LEGS = ('no-cluster', 'round-trip', 'repair', 'upgrade', 'from-source', 'pipelines')
CEILINGS = {'no-cluster': 1500, 'round-trip': 4800, 'repair': 4800, 'upgrade': 4800,
            'from-source': 7200, 'pipelines': 9900}


def create_args(name, image, volume, source, leg, nonce):
    cpus, memory = (2, '4g') if leg == 'no-cluster' else (4, '16g')
    return ['create', '--name', name, '--label', f'{LABEL}={nonce}',
            '--privileged', '--cgroupns=private', f'--cpus={cpus}', f'--memory={memory}',
            f'--memory-swap={memory}', '--pids-limit=8192', '--shm-size=256m',
            '--volume', f'{volume}:/var/lib/docker',
            '--env', f'MEMQL_SHA={source}', '--env', f'MEMQL_INSTALL_LEG={leg}', image]


class Fixture:
    def __init__(self, env, nonce):
        self.env, self.nonce = env, nonce
        self.name = 'memql-install-proof-' + nonce
        self.image = 'memql-install-fixture:' + nonce
        self.volume = self.name + '-data'
        self.container = None
        self.daemon = self.call('info', '--format', '{{.ID}}').strip()
        if not self.daemon:
            raise RuntimeError('local Docker did not report its identity')

    def call(self, *args, timeout=30, stream=False):
        result = subprocess.run(['docker', *args], env=self.env, text=True,
                                stdout=None if stream else subprocess.PIPE,
                                stderr=None if stream else subprocess.PIPE,
                                check=True, timeout=timeout)
        return result.stdout

    def same_daemon(self):
        if self.call('info', '--format', '{{.ID}}').strip() != self.daemon:
            raise RuntimeError('Docker daemon identity changed; refusing mutation')

    def owned(self, kind, name):
        self.same_daemon()
        try:
            rows = json.loads(self.call(kind, 'inspect', name))
        except subprocess.CalledProcessError as exc:
            if f'no such {kind}' in (exc.stderr or '').lower():
                return False
            raise
        if len(rows) != 1:
            raise RuntimeError('ambiguous fixture resource')
        labels = rows[0].get('Config', {}).get('Labels', {}) if kind in ('container', 'image') else rows[0].get('Labels', {})
        if (labels or {}).get(LABEL) != self.nonce:
            raise RuntimeError('resource is not owned by this installer test')
        if kind == 'container' and self.container and rows[0]['Id'] != self.container:
            raise RuntimeError('fixture container was replaced')
        if kind == 'container' and self.container is None:
            identity = rows[0]['Id']
            if not re.fullmatch(r'[a-f0-9]{64}', identity):
                raise RuntimeError('invalid recovered fixture container identity')
            self.container = identity
        return True

    def cleanup(self, evidence):
        errors = []
        if self.owned('container', self.name):
            for args in [('stop', '--time=30', self.container),
                         ('cp', self.container + ':/evidence/.', str(evidence)),
                         ('rm', '--force', '--volumes', self.container)]:
                try:
                    self.same_daemon()
                    self.call(*args, timeout=60)
                except (subprocess.SubprocessError, RuntimeError) as exc:
                    errors.append(str(exc))
        for kind, name in [('volume', self.volume), ('image', self.image)]:
            try:
                if self.owned(kind, name):
                    self.call(kind, 'rm', name, timeout=60)
            except (subprocess.SubprocessError, RuntimeError) as exc:
                errors.append(str(exc))
        if errors:
            raise RuntimeError('fixture cleanup incomplete: ' + '; '.join(errors))

    def execute(self, root, source, leg):
        self.same_daemon()
        self.call('build', '--label', f'{LABEL}={self.nonce}', '--tag', self.image,
                  str(root / 'scripts/install/ci'), timeout=900, stream=True)
        self.same_daemon()
        self.call('volume', 'create', '--label', f'{LABEL}={self.nonce}', self.volume)
        self.container = self.call(*create_args(self.name, self.image, self.volume,
                                                source, leg, self.nonce)).strip()
        if not re.fullmatch(r'[a-f0-9]{64}', self.container):
            raise RuntimeError('invalid fixture container identity')
        self.same_daemon()
        self.call('start', '--attach', self.container, timeout=CEILINGS[leg], stream=True)
        state = json.loads(self.call('inspect', '--format', '{{json .State}}', self.container))
        if state.get('Running') or state.get('OOMKilled') or state.get('ExitCode') != 0:
            raise RuntimeError('installer fixture did not exit successfully')


def local_endpoint():
    endpoint = os.environ.get('DOCKER_HOST')
    if not endpoint:
        rows = json.loads(subprocess.check_output(['docker', 'context', 'inspect'], text=True, timeout=15))
        endpoint = rows[0]['Endpoints']['docker']['Host']
    if not endpoint.startswith('unix:///'):
        raise RuntimeError('installer fixture requires a local Unix Docker socket')
    return endpoint


def interrupted(signum, _frame):
    raise RuntimeError(f'installer fixture interrupted by signal {signum}')


def start_reaper(item, evidence, ceiling):
    # Native step cancellation can escalate to SIGKILL. This separate session
    # holds a kernel pidfd for this exact parent (no PID reuse) and removes only
    # its nonce-labelled resources if the parent exits before finishing cleanup.
    parent = os.pidfd_open(os.getpid())
    record = {'nonce': item.nonce, 'daemon': item.daemon, 'endpoint': item.env['DOCKER_HOST'],
              'evidence': str(evidence), 'ceiling': ceiling}
    try:
        child = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), '--reaper', str(parent)],
                                 stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                 start_new_session=True, pass_fds=(parent,), text=True)
        child.stdin.write(json.dumps(record))
        child.stdin.close()
        if not select.select([child.stdout], [], [], 5)[0] or child.stdout.readline() != 'ready\n':
            child.terminate()
            child.wait(timeout=5)
            raise RuntimeError('installer cleanup supervisor did not become ready')
        child.stdout.close()
        return child
    finally:
        os.close(parent)


def reap(parent):
    record = json.load(sys.stdin)
    if not re.fullmatch(r'[a-f0-9]{32}', record['nonce']) or not record['endpoint'].startswith('unix:///'):
        raise RuntimeError('invalid installer cleanup identity')
    if not 0 < record['ceiling'] <= 12000:
        raise RuntimeError('invalid installer cleanup deadline')
    print('ready', flush=True)
    select.select([parent], [], [], record['ceiling'])
    os.close(parent)
    # The parent's private Docker config may already have been removed. No
    # registry credential or original home directory is needed for cleanup.
    with tempfile.TemporaryDirectory(prefix='memql-install-reaper-') as config:
        env = {key: value for key, value in os.environ.items()
               if key not in ('DOCKER_CONTEXT', 'DOCKER_CERT_PATH', 'DOCKER_TLS_VERIFY')}
        env.update(DOCKER_CONFIG=config, DOCKER_HOST=record['endpoint'])
        item = Fixture(env, record['nonce'])
        if item.daemon != record['daemon']:
            raise RuntimeError('installer cleanup daemon changed')
        evidence = Path(record['evidence'])
        receipt = {'formatVersion': 1, 'cleanupComplete': False}
        try:
            item.cleanup(evidence)
            receipt['cleanupComplete'] = True
        except Exception as exc:
            receipt['error'] = str(exc)
            raise
        finally:
            if evidence.is_dir():
                (evidence / 'reaper.json').write_text(json.dumps(receipt, indent=2) + '\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--leg', required=True, choices=LEGS)
    args = parser.parse_args()
    source = os.environ.get('MEMQL_SHA', '')
    if not re.fullmatch(r'[a-f0-9]{40}', source):
        raise RuntimeError('MEMQL_SHA must name the exact pushed source commit')
    root = Path(__file__).resolve().parents[3]
    if shutil.disk_usage(root).free < 64 * 1024**3:
        raise RuntimeError('installer fixture requires 64 GiB free working disk')
    evidence = root / '.memql-install-evidence' / args.leg
    evidence.mkdir(parents=True, mode=0o700, exist_ok=False)
    nonce = uuid.uuid4().hex
    result = {'formatVersion': 1, 'source': source, 'leg': args.leg, 'passed': False,
              'cleanupComplete': False, 'cpuLimit': 2 if args.leg == 'no-cluster' else 4,
              'memoryLimitMiB': 4096 if args.leg == 'no-cluster' else 16384}
    endpoint = local_endpoint()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, interrupted)
    with tempfile.TemporaryDirectory(prefix='.docker-fixture-', dir=root) as config:
        env = {key: value for key, value in os.environ.items()
               if key not in ('DOCKER_CONTEXT', 'DOCKER_CERT_PATH', 'DOCKER_TLS_VERIFY')}
        env.update(DOCKER_CONFIG=config, DOCKER_HOST=endpoint)
        fixture = Fixture(env, nonce)
        reaper = start_reaper(fixture, evidence, CEILINGS[args.leg] + 1050)
        try:
            fixture.execute(root, source, args.leg)
            result['passed'] = True
        finally:
            try:
                fixture.cleanup(evidence)
                result['cleanupComplete'] = True
                reaper.terminate()
                reaper.wait(timeout=5)
            finally:
                (evidence / 'fixture.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result))


if __name__ == '__main__':
    if len(sys.argv) == 3 and sys.argv[1] == '--reaper':
        reap(int(sys.argv[2]))
    else:
        main()
