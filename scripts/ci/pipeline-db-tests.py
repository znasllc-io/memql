#!/usr/bin/env python3
"""Run a native pipeline shard with the CI planner's per-package budgets.

The pipeline selects packages, owns sharding and provisions the database. This
command only partitions that exact selection by the shared class table. It
never discovers additional packages or retries a test. Keeping package budgets
prevents the slower engine suite from granting every DB test a ten-minute cap.
"""
import json
import os
from pathlib import Path
import re
import subprocess
import sys

import yaml

MODULE = "github.com/znasllc-io/memql/"


def read_classes(workflow):
    steps = workflow["jobs"]["plan"]["steps"]
    tables = [s["env"]["CLASSES"] for s in steps if s.get("id") == "plan"]
    if len(tables) != 1:
        raise ValueError("expected one CI planner class table")
    classes = []
    for line in tables[0].splitlines():
        words = line.split()
        if not words or words[0].startswith("#"):
            continue
        if len(words) != 6:
            raise ValueError("invalid class table row")
        lane, name, trees, timeout, mode, shards = words
        if lane != "db":
            continue
        if not re.fullmatch(r"[1-9][0-9]*s", timeout):
            raise ValueError("invalid test timeout")
        if mode != "-" and any(m not in ("serial", "uncached") for m in mode.split(",")):
            raise ValueError("unknown test mode")
        classes.append((name, [] if trees == "-" else trees.split(","), timeout))
    if sum(not c[1] for c in classes) != 1 or len({c[0] for c in classes}) != len(classes):
        raise ValueError("DB classes need unique names and exactly one default")
    return classes


def commands(packages, classes):
    if not packages or len(set(packages)) != len(packages):
        raise ValueError("expected a nonempty, unique package selection")
    selected = {name: [] for name, _, _ in classes}
    default = next(name for name, trees, _ in classes if not trees)
    for package in packages:
        directory = package.removeprefix(MODULE).removeprefix("./")
        if not (package.startswith(MODULE) or package.startswith("./")) or not re.fullmatch(r"[A-Za-z0-9_-]+(?:/[A-Za-z0-9_.+-]+)*", directory) or any(p in (".", "..") for p in directory.split("/")):
            raise ValueError("expected explicit repository packages, without patterns or flags")
        matches = [(len(tree), name) for name, trees, _ in classes for tree in trees if directory == tree or directory.startswith(tree + "/")]
        name = max(matches)[1] if matches else default
        selected[name].append(package)
    # Every invocation is serial beside this shard's one Postgres, as before.
    # -count=1 also preserves the pipeline's always-uncached execution policy.
    return [(name, ["go", "test", "-count=1", "-p=1", "-timeout=" + timeout, *selected[name]]) for name, _, timeout in classes if selected[name]]


def run(packages, classes):
    for name, command in commands(packages, classes):
        print(json.dumps({"class": name, "command": command}), flush=True)
        result = subprocess.run(command, check=False)
        if result.returncode:
            return result.returncode if result.returncode > 0 else 1
    return 0


def main():
    if os.environ.get("MEMQL_REQUIRE_DB") != "1" or not os.environ.get("MEMQL_DATABASE_DSN"):
        raise ValueError("database-required execution and its DSN must be explicit")
    workflow = yaml.safe_load(Path(".github/workflows/ci.yml").read_text())
    return run(os.environ.get("MEMQL_PACKAGES", "").split(), read_classes(workflow))


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError) as error:
        print("pipeline DB tests: " + str(error), file=sys.stderr)
        sys.exit(1)
