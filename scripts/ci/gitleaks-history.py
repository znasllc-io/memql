#!/usr/bin/env python3
"""Scan pinned Git history in bounded Gitleaks processes, retaining coverage.

Scheduling, fetching and publication belong to the caller. Every commit reachable
from the selected head and the captured tags is scanned. Process failure or an
incomplete report can never produce a successful coverage receipt.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import time

VERSION = "8.30.1"


def git(source, *args):
    return subprocess.check_output(["git", "-C", str(source), *args], text=True).strip()


def write_json(path, value):
    temporary = path.with_suffix(path.suffix + ".pending")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


def run_scanner(command, log, remaining):
    # The scanner starts git. A timeout must end that owned process group too,
    # without touching any process belonging to another run on the machine.
    with subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT,
                          start_new_session=True) as process:
        try:
            return process.wait(timeout=remaining)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(process.pid, signal.SIGTERM)
                process.wait(timeout=2)
            except ProcessLookupError:
                pass
            except subprocess.TimeoutExpired:
                pass
            # A parent can exit while one child ignores TERM.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
            raise


def scan(source, head, output, config, binary, batch_size, timeout):
    summary = {"status": "incomplete", "version": VERSION, "completedCommits": 0,
               "totalCommits": 0, "findings": 0, "batches": []}
    deadline = time.monotonic() + timeout
    findings = []
    try:
        if git(source, "rev-parse", "--is-shallow-repository") != "false":
            raise RuntimeError("full-history scanning requires a complete Git history")
        head = git(source, "rev-parse", "--verify", head + "^{commit}")
        if not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", head):
            raise RuntimeError("head is not a full commit identity")
        tags = git(source, "for-each-ref", "--format=%(refname) %(objectname)", "refs/tags")
        (output / "tags.txt").write_text(tags + "\n")
        roots = [head, *(line.split()[1] for line in tags.splitlines())]
        commits = git(source, "rev-list", "--topo-order", *roots).splitlines()
        if not commits or len(set(commits)) != len(commits):
            raise RuntimeError("empty or repeated history inventory")
        inventory = "\n".join(commits) + "\n"
        (output / "commits.txt").write_text(inventory)
        # Freeze the selected config, just as refs were frozen above.
        frozen_config = output / "config.toml"
        shutil.copyfile(config, frozen_config)
        installed = subprocess.check_output([binary, "version"], text=True).strip()
        if installed.removeprefix("v") != VERSION:
            # `go install ...@v8.30.1` omits release ldflags. In that case use
            # Go's embedded module identity, never accept an unknown version.
            metadata = subprocess.check_output(["go", "version", "-m", binary], text=True)
            expected = f"mod\tgithub.com/zricethezav/gitleaks/v8\tv{VERSION}\t"
            if not any(line.strip().startswith(expected) for line in metadata.splitlines()) or "\n\t=>" in metadata:
                raise RuntimeError(f"expected Gitleaks {VERSION}, got {installed}")
        summary.update(sourceCommit=head, totalCommits=len(commits),
                       inventorySHA256=hashlib.sha256(inventory.encode()).hexdigest(),
                       configSHA256=hashlib.sha256(frozen_config.read_bytes()).hexdigest())
        write_json(output / "summary.json", summary)
        for offset in range(0, len(commits), batch_size):
            batch = commits[offset:offset + batch_size]
            number = len(summary["batches"])
            report = output / f"batch-{number:05d}.json"
            # Every parent is already in the inventory. Diff each merge
            # against its first parent to cover newly introduced merge bytes
            # without re-scanning all intervening main changes against every
            # side parent. This is NOT --first-parent history traversal.
            options = "--no-walk=unsorted --root --diff-merges=first-parent --no-ext-diff --no-textconv " + " ".join(batch)
            command = [binary, "git", str(source), "--no-banner", "--redact=100",
                       f"--config={frozen_config}", f"--log-opts={options}",
                       "--report-format=json", f"--report-path={report}"]
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise RuntimeError("history scan deadline expired")
            with (output / f"batch-{number:05d}.log").open("x") as log:
                code = run_scanner(command, log, remaining)
            if code not in (0, 1):
                raise RuntimeError(f"batch {number} scanner failed with exit {code}")
            evidence = json.loads(report.read_text())
            if not isinstance(evidence, list) or any(not isinstance(item, dict) for item in evidence):
                raise RuntimeError(f"batch {number} has no valid findings array")
            if bool(evidence) != (code == 1):
                raise RuntimeError(f"batch {number} result disagrees with its report")
            if any(item.get("Commit") not in batch for item in evidence):
                raise RuntimeError(f"batch {number} reported a commit outside its inventory")
            findings.extend(evidence)
            summary["completedCommits"] += len(batch)
            summary["findings"] = len(findings)
            summary["batches"].append({"firstIndex": offset, "commits": len(batch),
                                       "exit": code,
                                       "reportSHA256": hashlib.sha256(report.read_bytes()).hexdigest()})
            write_json(output / "summary.json", summary)
            print(f"scanned {summary['completedCommits']}/{len(commits)} commits", file=sys.stderr, flush=True)
        summary["status"] = "findings" if findings else "clean"
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        summary["error"] = str(exc)
        print(str(exc), file=sys.stderr)
    write_json(output / "findings.json", findings)
    write_json(output / "summary.json", summary)
    return summary


def positive(value):
    result = int(value)
    if result < 1:
        raise argparse.ArgumentTypeError("must be positive")
    return result


class ParametersInvalid(ValueError):
    pass


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise ParametersInvalid(message)


def emit(result=None, error=None, code=0):
    print(json.dumps({"ok": code == 0, "changed": False,
                      "capability": "security.scanHistory", "result": result or {},
                      "error": {"code": code, "message": error} if code else None}))
    return code


def main():
    parser = Parser(description=__doc__)
    parser.add_argument("--source", type=Path, default=Path.cwd())
    parser.add_argument("--head", default="HEAD")
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--config", type=Path, default=Path(".gitleaks.toml"))
    parser.add_argument("--gitleaks", default="gitleaks")
    parser.add_argument("--batch-size", type=positive, default=32)
    parser.add_argument("--timeout", type=positive, default=5400)
    try:
        args = parser.parse_args()
        args.output_dir.mkdir(parents=True, exist_ok=True)
        if any(args.output_dir.iterdir()):
            raise ParametersInvalid("output directory must be empty; retain each attempt's evidence separately")
    except ParametersInvalid as exc:
        return emit(error=str(exc), code=2)
    except OSError as exc:
        return emit(error=str(exc), code=5)
    try:
        summary = scan(args.source.resolve(), args.head, args.output_dir.resolve(),
                       args.config.resolve(), args.gitleaks, args.batch_size, args.timeout)
    except OSError as exc:
        # Evidence persistence can itself fail (for example, a full disk).
        # No final coverage receipt is permission to report success.
        return emit(error=str(exc), code=5)
    if summary["status"] != "clean":
        return emit(result=summary, error=summary.get("error", "secret findings require review"), code=5)
    return emit(result=summary)


if __name__ == "__main__":
    sys.exit(main())
