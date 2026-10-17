#!/usr/bin/env python3
"""Materialize the candidate and its pinned engine as owned sibling checkouts.

The manifest chooses every command and platform. This helper only prepares two
exact commits inside the attempt's checkout; it never writes ../memql or uses
the operator's Git credentials. --engine-source supplies an offline local clone.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys


def envelope(code, message=None, result=None):
    print(json.dumps({"ok": code == 0, "capability": "cockpit.pipeline.prepare",
                      "changed": code == 0, "result": result,
                      "error": None if code == 0 else {"code": code, "message": message}}))


class Parser(argparse.ArgumentParser):
    def error(self, message):
        envelope(2, message)
        raise SystemExit(2)


def git(*args, cwd, capture=False, trusted_source=None):
    env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    env.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
               GIT_TERMINAL_PROMPT="0", GIT_ALLOW_PROTOCOL="https:file")
    config = ["-c", "credential.helper=", "-c", "core.hooksPath=" + os.devnull,
              "-c", "safe.directory=" + str(cwd)]
    if trusted_source:
        config += ["-c", "safe.directory=" + trusted_source]
    return subprocess.run(["git", *config, *args], cwd=cwd, env=env, check=True, text=True,
                          stdout=subprocess.PIPE if capture else sys.stderr, stderr=sys.stderr)


def parse_pin(contents):
    rows = [line.strip() for line in contents.splitlines()
            if line.strip() and not line.lstrip().startswith("#")]
    if len(rows) != 1 or not re.fullmatch(r"[0-9a-f]{40}", rows[0]):
        raise ValueError(".github/memql-pin must contain exactly one full commit SHA")
    return rows[0]


def pin_from(root):
    return parse_pin((root / ".github/memql-pin").read_text())


def checkout(source, revision, destination):
    destination.mkdir()
    git("init", "-q", cwd=destination)
    git("fetch", "-q", "--depth=1", "--no-tags", source, revision, cwd=destination,
        trusted_source=source if Path(source).is_absolute() else None)
    git("checkout", "-q", "--detach", "FETCH_HEAD", cwd=destination)
    actual = git("rev-parse", "HEAD", cwd=destination, capture=True).stdout.strip()
    if actual != revision:
        raise ValueError("checkout did not resolve to the requested commit")


def prepare(root, engine_source=None):
    root = root.resolve()
    candidate = git("rev-parse", "HEAD", cwd=root, capture=True).stdout.strip()
    pin = parse_pin(git("show", candidate + ":.github/memql-pin", cwd=root, capture=True).stdout)
    # Refuse reuse, including a symlink. The outer runner owns attempt cleanup.
    scratch = root / ".memql-ci"
    scratch.mkdir()
    checkout(str(root), candidate, scratch / "memql-cockpit")
    checkout(str(engine_source.resolve()) if engine_source else "https://github.com/znasllc-io/memql.git",
             pin, scratch / "memql")
    return {"ok": True, "candidate": candidate, "engine": pin,
            "directory": str(scratch / "memql-cockpit")}


def main():
    parser = Parser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--engine-source", type=Path)
    args = parser.parse_args()
    try:
        result = prepare(args.root, args.engine_source)
    except FileExistsError:
        envelope(3, "owned scratch already exists; use a fresh attempt")
        return 3
    except (ValueError, OSError, subprocess.SubprocessError) as exc:
        envelope(5, str(exc))
        return 5
    result.pop("ok", None)
    envelope(0, result=result)
    return 0


if __name__ == "__main__":
    sys.exit(main())
