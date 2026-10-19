#!/usr/bin/env python3
"""Produce bounded native repository-change proposals; never write to GitHub."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        raise ValueError('GitHub API redirected outside the requested endpoint')


def github(endpoint, missing=False):
    if not endpoint.startswith('/repos/') or any(c in endpoint for c in '\r\n'):
        raise ValueError('expected a repository API path')
    headers = {
        'Accept': 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28',
        'User-Agent': 'MemQL-repository-proposal'}
    repository = os.environ.get('MEMQL_REPOSITORY', '')
    token = os.environ.get('MEMQL_GITHUB_READ_TOKEN', '')
    if token and re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository) and endpoint.startswith('/repos/' + repository + '/'):
        headers['Authorization'] = 'Bearer ' + token
    request = urllib.request.Request('https://api.github.com' + endpoint, headers=headers)
    try:
        with urllib.request.build_opener(NoRedirect()).open(request, timeout=30) as response:
            body = response.read(2 * 1024 * 1024 + 1)
    except urllib.error.HTTPError as error:
        if missing and error.code == 404:
            return None
        raise ValueError(f'GitHub metadata could not be read (HTTP {error.code})') from None
    if len(body) > 2 * 1024 * 1024:
        raise ValueError('GitHub metadata exceeds its read bound')
    return json.loads(body)


def git(root, *args):
    return subprocess.check_output(['git', '-C', str(root), *args], timeout=30).decode('utf-8')


def regular_bytes(root, name):
    file = root / name
    for part in [file, *file.parents]:
        if part == root:
            break
        if part.is_symlink():
            raise ValueError('proposal source crosses a symlink')
    if not file.exists():
        return None
    if not stat.S_ISREG(file.stat().st_mode) or file.stat().st_mode & 0o111:
        raise ValueError('proposal source must be a non-executable regular file')
    data = file.read_bytes()
    if len(data) > 1024 * 1024 or b'\0' in data:
        raise ValueError('proposal source exceeds its text bound')
    data.decode('utf-8')
    return data


def replacement(name, before, after):
    if after is None:
        raise ValueError('repository proposals cannot delete files')
    return {'path': name, 'baseSha256': hashlib.sha256(before).hexdigest() if before is not None else '',
            'content': after.decode('utf-8')}


def empty(commit, key, title):
    return {'schema': 1, 'commit': commit, 'key': key, 'title': title, 'body': '', 'files': []}


def version(tag):
    match = re.fullmatch(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', tag)
    if not match:
        raise ValueError('engine watch requires a stable vMAJOR.MINOR.PATCH release pin')
    return tuple(map(int, match.groups()))


def engine_watch(root, repository, commit, read=github):
    proposal = empty(commit, 'engine-current', 'Engine pin is current')
    original = regular_bytes(root, 'product.env')
    if original is None:  # Unstamped template.
        return proposal
    text = original.decode('utf-8')
    assignments = list(re.finditer(r'^ENGINE_REF=([^\r\n]+)$', text, re.MULTILINE))
    if len(assignments) != 1:
        raise ValueError('product.env must have one literal ENGINE_REF assignment')
    current = assignments[0].group(1)
    current_version = version(current)
    latest = read('/repos/znasllc-io/memql/releases/latest')
    if not isinstance(latest, dict) or latest.get('draft') is not False or latest.get('prerelease') is not False:
        raise ValueError('latest engine release is not a published stable release')
    tag = latest.get('tag_name', '')
    if version(tag) <= current_version:
        return proposal
    # Existing and deliberately closed proposals keep their branch. Reading
    # this public metadata grants no write; the driver independently checks it.
    branch = 'engine-bump/' + tag
    if read('/repos/' + repository + '/git/ref/heads/' + urllib.parse.quote(branch, safe='/'), missing=True) is not None:
        return proposal
    match = assignments[0]
    updated = (text[:match.start(1)] + tag + text[match.end(1):]).encode('utf-8')
    proposal.update(key=tag, title='engine: propose ' + tag,
        body=f'The engine published {tag}; this product pins {current}.\n\n'
             'This draft updates product.env. Before merging, review the release notes, '
             'resolve and update the engine image digests under deploy/, and pass the native pipeline. '
             'The proposal does not deploy or merge automatically.',
        files=[replacement('product.env', original, updated)])
    return proposal


def manifest_paths(root):
    modules = sorted(name for name in git(root, 'ls-files', '-z').split('\0')
                     if name and Path(name).name == 'go.mod')
    if not modules or len(modules) > 100:
        raise ValueError('expected a bounded tracked module inventory')
    paths = {'go.work.sum'}
    for module in modules:
        regular_bytes(root, module)
        paths.update([module, str(Path(module).with_name('go.sum'))])
    return modules, sorted(paths)


def content_digest(snapshot):
    digest = hashlib.sha256()
    for name, body in sorted(snapshot.items()):
        digest.update(name.encode() + b'\0')
        digest.update(b'absent' if body is None else hashlib.sha256(body).digest())
    return digest.digest()


def converge(root, modules, paths, run=subprocess.run):
    env = dict(os.environ, GOTOOLCHAIN='local', GOWORK='off')
    env.pop('MEMQL_GITHUB_READ_TOKEN', None)
    deadline = time.monotonic() + 20 * 60
    for _round in range(5):
        before = {name: regular_bytes(root, name) for name in paths}
        for module in modules:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise ValueError('tidy exceeded its execution bound')
            run(['go', 'mod', 'tidy'], cwd=root / Path(module).parent, env=env,
                timeout=remaining, check=True)
        run(['go', 'work', 'sync'], cwd=root, env=dict(env, GOWORK=str(root / 'go.work')),
            timeout=max(1, deadline - time.monotonic()), check=True)
        after = {name: regular_bytes(root, name) for name in paths}
        if content_digest(before) == content_digest(after):
            return after
    raise ValueError('tidy/sync did not converge in five rounds')


def dependabot_tidy(root, repository, commit, read=github, tidy=converge):
    proposal = empty(commit, 'tidy', 'chore: tidy modules after the dependency bump')
    if os.environ.get('MEMQL_EVENT') != 'pull_request':
        return proposal
    prs = read(f'/repos/{repository}/commits/{commit}/pulls?per_page=100')
    if not isinstance(prs, list) or len(prs) >= 100:
        raise ValueError('pull-request inventory is incomplete')
    eligible = [pr for pr in prs if pr.get('state') == 'open' and
        pr.get('user', {}).get('login') == 'dependabot[bot]' and
        pr.get('head', {}).get('sha') == commit and
        pr.get('head', {}).get('repo', {}).get('full_name') == repository and
        pr.get('head', {}).get('ref', '').startswith('dependabot/go_modules/')]
    if not eligible:
        return proposal
    if len(eligible) != 1:
        raise ValueError('commit belongs to ambiguous maintenance pull requests')
    number = eligible[0].get('number')
    if not isinstance(number, int) or number <= 0:
        raise ValueError('invalid maintenance pull request')
    modules, paths = manifest_paths(root)
    changed = []
    for page in range(1, 4):
        files = read(f'/repos/{repository}/pulls/{number}/files?per_page=100&page={page}')
        if not isinstance(files, list) or len(files) > 100:
            raise ValueError('invalid pull-request file inventory')
        for file in files:
            changed.append(file.get('filename'))
            if file.get('previous_filename'):
                changed.append(file['previous_filename'])
        if len(files) < 100:
            break
    else:
        raise ValueError('pull-request file inventory is incomplete')
    if not changed or any(name not in paths for name in changed):
        raise ValueError('maintenance pull request changes a non-manifest path')
    before = {name: regular_bytes(root, name) for name in paths}
    after = tidy(root, modules, paths)
    touched = set(git(root, 'diff', '--name-only', '-z').split('\0')) - {''}
    untracked = set(git(root, 'ls-files', '--others', '--exclude-standard', '-z').split('\0')) - {''}
    if (touched | untracked) - set(paths):
        raise ValueError('tidy changed a path outside its manifest inventory')
    proposal['files'] = [replacement(name, before[name], after[name]) for name in paths if before[name] != after[name]]
    proposal['body'] = 'Native Go module tidy and workspace sync, converged by file contents. No repository code was executed by the tidy operation.'
    return proposal


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('kind', choices=['engine-watch', 'dependabot-tidy', 'tidy-paths'])
    parser.add_argument('--output', default='.reports/repository-proposal.json')
    args = parser.parse_args()
    root = Path.cwd().resolve()
    if args.kind == 'tidy-paths':
        print(json.dumps(manifest_paths(root)[1])); return
    repository, commit = os.environ.get('MEMQL_REPOSITORY', ''), os.environ.get('MEMQL_SHA', '')
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository) or not re.fullmatch('[a-f0-9]{40}', commit):
        raise ValueError('native pipeline repository and exact commit are required')
    if git(root, 'rev-parse', 'HEAD').strip() != commit:
        raise ValueError('checkout differs from the native pipeline source')
    proposal = (engine_watch if args.kind == 'engine-watch' else dependabot_tidy)(root, repository, commit)
    body = json.dumps(proposal, ensure_ascii=False, indent=2).encode('utf-8') + b'\n'
    if len(body) > 4 * 1024 * 1024:
        raise ValueError('proposal exceeds its artifact bound')
    output = Path(args.output)
    if output.is_absolute() or '..' in output.parts or output.suffix != '.json':
        raise ValueError('output must be a relative JSON artifact path')
    for part in [output, *output.parents]:
        if part.is_symlink(): raise ValueError('output crosses a symlink')
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_bytes(body)
    print(f'Prepared {len(proposal["files"])} file change(s); no repository write performed.')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        raise SystemExit('Repository proposal failed: ' + str(error)) from None
