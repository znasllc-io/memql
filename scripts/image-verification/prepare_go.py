#!/usr/bin/env python3
"""Extract one previously downloaded, officially checksum-pinned Go archive.

No network, system-wide installation, package manager, or shell invocation.
The final owner block may download the fixed official URL after review.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import tarfile

URL = "https://go.dev/dl/go1.27.3.linux-amd64.tar.gz"
SHA256 = "65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40"
SIZE = 70588678


def extract(archive, destination):
    if archive.is_symlink() or archive.stat().st_size != SIZE:
        raise ValueError("The official Go archive size is wrong")
    if hashlib.sha256(archive.read_bytes()).hexdigest() != SHA256:
        raise ValueError("The official Go archive checksum is wrong")
    if not destination.is_absolute() or ".." in destination.parts or any(p.is_symlink() for p in [destination, *destination.parents]):
        raise ValueError("The toolchain destination must be a new canonical directory")
    destination.mkdir(mode=0o700, exist_ok=False)
    with tarfile.open(archive, "r:gz") as source:
        members = source.getmembers()
        if len(members) > 50000 or sum(m.size for m in members) > 1 << 30:
            raise ValueError("The official Go archive exceeds the extraction bound")
        seen = set()
        for member in members:
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts or not path.parts or path.parts[0] != "go" or str(path) in seen or not (member.isdir() or member.isfile()):
                raise ValueError("The official Go archive has an unsupported entry")
            seen.add(str(path))
        for member in members:
            path = destination / member.name
            if member.isdir():
                path.mkdir(mode=0o700, parents=True, exist_ok=True)
            else:
                path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o700 if member.mode & 0o111 else 0o600)
                with os.fdopen(fd, "wb") as out, source.extractfile(member) as inp:
                    remaining = member.size
                    while remaining:
                        chunk = inp.read(min(1 << 20, remaining))
                        if not chunk:
                            raise ValueError("The Go archive entry was truncated")
                        out.write(chunk)
                        remaining -= len(chunk)
    binary = destination / "go/bin/go"
    return {"path":str(binary), "sha256":hashlib.sha256(binary.read_bytes()).hexdigest(), "archiveSHA256":SHA256, "source":URL}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--destination", type=Path, required=True)
    args = parser.parse_args()
    try:
        result = extract(args.archive, args.destination)
        print(json.dumps({"ok":True,"capability":"image-verification.prepare-go","changed":True,"result":result,"error":None}))
        return 0
    except (OSError, ValueError, tarfile.TarError) as error:
        print(json.dumps({"ok":False,"capability":"image-verification.prepare-go","changed":args.destination.exists(),"result":{},"error":{"code":5,"message":str(error)}}))
        return 5


if __name__ == "__main__":
    raise SystemExit(main())
