#!/usr/bin/env python3
"""Package an explicitly reviewed case selection, never a recursive workspace.

The selection JSON is data: {caseId, snapshotId, files:[{source,path,mediaType,
provenance}], omissions:[], privacyReview:{reviewer, scope}}. Relative sources
resolve inside --root. Archives are private evidence; this tool never uploads
or publishes. Human review covers free text/media; structural checks cannot
prove that an image or arbitrary prose contains no private information.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import stat
import tempfile
import zipfile

MAX_FILES = 200
MAX_FILE = 20 * 1024 * 1024
MAX_TOTAL = 100 * 1024 * 1024
FORBIDDEN = {'.git', 'node_modules', '.venv', 'venv', '__pycache__', '.cache', 'credentials', '.env', '.ssh', '.aws', '.azure', '.gnupg', '.kube', '.codex', 'credentials.json', 'auth.json'}
PRIVATE_KEYS = {'address', 'streetaddress', 'encryptedaddress', 'secretsettings', 'accesstoken', 'refreshtoken', 'authorization', 'apikey', 'password'}


def safe_name(name):
    p = PurePosixPath(name)
    if not name or p.is_absolute() or str(p) != name or '..' in p.parts or '\\' in name or any(x.lower() in FORBIDDEN or x.lower().startswith('.env.') for x in p.parts):
        raise ValueError('unsafe or excluded archive path')
    return name


def private_fields(value):
    if isinstance(value, dict):
        return any(k.lower().replace('_', '').replace('-', '') in PRIVATE_KEYS or private_fields(v) for k, v in value.items())
    if isinstance(value, list):
        return any(private_fields(v) for v in value)
    return False


def build(root, selection, output):
    root, output = Path(root).resolve(), Path(output)
    if output.exists():
        raise ValueError('refusing to replace an earlier evidence snapshot')
    review = selection.get('privacyReview', {})
    if not review.get('reviewer') or not review.get('scope'):
        raise ValueError('explicit privacy review of the selected bytes is required')
    if not selection.get('caseId') or not selection.get('snapshotId'):
        raise ValueError('case and snapshot identifiers are required')
    entries, inventory, total = {}, [], 0
    for item in selection.get('files', []):
        name = safe_name(item['path'])
        relative = safe_name(item['source'])
        source = root / relative
        # Refuse even in-root symlinks: the reviewed source must be the exact file.
        if any(p.is_symlink() for p in [source, *source.parents]) or not source.resolve().is_relative_to(root):
            raise ValueError('source escapes the root or is a symlink')
        if name == 'manifest.json' or name in entries or len(entries) >= MAX_FILES:
            raise ValueError('duplicate, reserved or excessive archive member')
        info = source.stat()
        if not stat.S_ISREG(info.st_mode) or info.st_size > MAX_FILE:
            raise ValueError('source is not a bounded regular file')
        data = source.read_bytes()
        total += len(data)
        if len(data) > MAX_FILE or total > MAX_TOTAL:
            raise ValueError('archive exceeds its byte limits')
        if name.endswith(('.json', '.jsonl')):
            docs = [json.loads(line) for line in data.decode().splitlines() if line.strip()] if name.endswith('.jsonl') else [json.loads(data)]
            if any(private_fields(doc) for doc in docs):
                raise ValueError('structured evidence includes private fields')
        if not item.get('mediaType') or not item.get('provenance'):
            raise ValueError('every selected artifact needs media type and provenance')
        entries[name] = data
        inventory.append({'path': name, 'bytes': len(data), 'sha256': hashlib.sha256(data).hexdigest(), 'mediaType': item['mediaType'], 'provenance': item['provenance']})
    if not {'README.md', 'case.json'} <= entries.keys():
        raise ValueError('README.md and case.json are required')
    manifest = {'schemaVersion': 1, 'caseId': selection['caseId'], 'snapshotId': selection['snapshotId'], 'visibility': 'private-evidence; publication not authorized', 'privacyReview': review, 'limits': {'files': MAX_FILES, 'fileBytes': MAX_FILE, 'uncompressedBytes': MAX_TOTAL, 'compressedBytes': MAX_TOTAL}, 'files': inventory, 'omissions': selection.get('omissions', []), 'manifestHash': 'Excluded from its own inventory; archive hash is in the external receipt.'}
    entries['manifest.json'] = (json.dumps(manifest, indent=2) + '\n').encode()
    if total + len(entries['manifest.json']) > MAX_TOTAL:
        raise ValueError('manifest exceeds total archive limit')
    output.parent.mkdir(parents=True, exist_ok=True)
    # Stage and verify before installing. Hard-link installation refuses races
    # that would replace a previously retained immutable snapshot.
    with tempfile.NamedTemporaryFile(dir=output.parent, suffix='.zip') as temp:
        with zipfile.ZipFile(temp.name, 'w', zipfile.ZIP_DEFLATED) as z:
            for name in sorted(entries):
                meta = zipfile.ZipInfo(name, date_time=(2026, 1, 1, 0, 0, 0))
                meta.compress_type = zipfile.ZIP_DEFLATED
                meta.external_attr = 0o600 << 16
                z.writestr(meta, entries[name])
        verify(temp.name)
        data = Path(temp.name).read_bytes()
        if len(data) > MAX_TOTAL:
            raise ValueError('compressed archive exceeds limit')
        os.link(temp.name, output)
    receipt = {'schemaVersion':1, 'caseId':selection['caseId'], 'snapshotId':selection['snapshotId'], 'archive':output.name, 'sha256':hashlib.sha256(data).hexdigest(), 'compressedBytes':len(data), 'uncompressedBytes':sum(map(len,entries.values())), 'members':len(entries), 'verification':'CRC, bounded safe inventory, exact bytes and SHA-256 passed; content review remains human-attributed'}
    receipt_path = output.with_suffix('.receipt.json')
    with receipt_path.open('x') as f:
        json.dump(receipt, f, indent=2); f.write('\n')
    return receipt


def verify(path):
    with zipfile.ZipFile(path) as z:
        infos = z.infolist()
        names = [safe_name(i.filename) for i in infos]
        if len(names) != len(set(names)) or len(names) > MAX_FILES + 1 or sum(i.file_size for i in infos) > MAX_TOTAL or any(i.file_size > MAX_FILE or stat.S_ISLNK(i.external_attr >> 16) for i in infos):
            raise ValueError('unsafe archive inventory')
        if z.testzip() is not None:
            raise ValueError('archive CRC failed')
        manifest = json.loads(z.read('manifest.json'))
        inventory = manifest['files']
        if len({i['path'] for i in inventory}) != len(inventory) or {i['path'] for i in inventory} | {'manifest.json'} != set(names):
            raise ValueError('manifest does not cover every member exactly once')
        for item in inventory:
            data = z.read(item['path'])
            if len(data) != item['bytes'] or hashlib.sha256(data).hexdigest() != item['sha256']:
                raise ValueError('artifact checksum mismatch')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--verify')
    parser.add_argument('--root')
    parser.add_argument('--selection')
    parser.add_argument('--output')
    args = parser.parse_args()
    if args.verify:
        verify(args.verify)
        print('Archive inventory and checksums verified.')
    elif args.root and args.selection and args.output:
        print(json.dumps(build(args.root, json.loads(Path(args.selection).read_text()), args.output), indent=2))
    else:
        parser.error('provide --verify or --root, --selection and --output')
