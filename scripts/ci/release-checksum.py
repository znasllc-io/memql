#!/usr/bin/env python3
"""Write a checksum beside one completed computer-use release binary."""

import argparse
import hashlib
from pathlib import Path
import stat


def checksum(platform: str, root: Path) -> Path:
    name = "memql-computeruse-" + platform
    binary = root / "bin" / name
    if not stat.S_ISREG(binary.lstat().st_mode):
        raise ValueError("the release binary must be a regular file")
    digest = hashlib.sha256()
    with binary.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    output = root / "dist" / (name + ".sha256")
    output.parent.mkdir(exist_ok=True)
    output.write_text(f"{digest.hexdigest()}  {name}\n", encoding="ascii")
    return output


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("platform", choices=("darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"))
    args = parser.parse_args()
    print(checksum(args.platform, Path.cwd()))
