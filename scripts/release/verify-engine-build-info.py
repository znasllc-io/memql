#!/usr/bin/env python3
"""Validate go version -m output as data; never execute the inspected binary."""

import argparse
from pathlib import Path
import shlex
import sys


def verify(text, version, source_sha, node):
    settings = {}
    paths = []
    for line in text.splitlines():
        if line.startswith("\tpath\t"):
            paths.append(line.removeprefix("\tpath\t"))
        if line.startswith("\tbuild\t"):
            key, sep, value = line.removeprefix("\tbuild\t").partition("=")
            if not sep or key in settings:
                raise ValueError("missing or duplicate build setting")
            # Go quotes settings with whitespace. Decode that outer quoted
            # value before parsing the linker's individual flags below.
            if value.startswith('"'):
                decoded = shlex.split(value)
                if len(decoded) != 1:
                    raise ValueError("invalid quoted build setting")
                value = decoded[0]
            settings[key] = value
    if paths != ["github.com/znasllc-io/memql"]:
        raise ValueError("binary is not the engine main package")
    for key, value in {"GOOS": "linux", "GOARCH": "amd64", "-tags": node}.items():
        if settings.get(key) != value:
            raise ValueError(f"binary {key} does not match the release")
    flags = shlex.split(settings.get("-ldflags", ""))
    assignments = []
    index = 0
    while index < len(flags):
        flag = flags[index]
        if flag == "-X":
            index += 1
            if index == len(flags):
                raise ValueError("missing linker assignment")
            assignments.append(flags[index])
        elif flag.startswith("-X="):
            assignments.append(flag[3:])
        index += 1
    for field, expected in {"release": version, "commit": source_sha}.items():
        prefix = f"github.com/znasllc-io/memql/core/buildinfo.{field}="
        values = [item[len(prefix):] for item in assignments if item.startswith(prefix)]
        if values != [expected]:
            raise ValueError(f"binary buildinfo.{field} must match exactly once")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--info", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--source-sha", required=True)
    parser.add_argument("--node", required=True)
    args = parser.parse_args()
    try:
        verify(Path(args.info).read_text(), args.version, args.source_sha, args.node)
    except (OSError, ValueError) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
