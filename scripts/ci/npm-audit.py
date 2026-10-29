#!/usr/bin/env python3
"""Audit each tracked npm lockfile without installing or running package scripts.

With --report-only (release runs) known vulnerabilities are recorded and
reported as warnings without failing: the release's pull request already ran
this audit, and an advisory published since must not block shipping (owner
decision, 2026-10-29). A report that cannot be read is still no audit and fails.
"""
import argparse
import json
from pathlib import Path
import subprocess
import sys


def audit(report_dir: Path, report_only: bool = False) -> int:
    paths = subprocess.check_output(
        ["git", "ls-files", "-z", "--", "package-lock.json", "**/package-lock.json"],
        text=True,
    ).split("\0")
    locks = sorted({Path(path) for path in paths if path})
    if not locks:
        raise RuntimeError("no tracked npm lockfiles; no audit was performed")
    report_dir.mkdir(parents=True, exist_ok=True)
    results = []
    for lock in locks:
        name = str(lock.parent).replace("/", "__") if lock.parent != Path(".") else "root"
        result = subprocess.run(
            ["npm", "audit", "--package-lock-only", "--ignore-scripts", "--json"],
            cwd=lock.parent, text=True, capture_output=True, timeout=300,
        )
        (report_dir / (name + ".json")).write_text(result.stdout)
        (report_dir / (name + ".stderr.txt")).write_text(result.stderr)
        report = json.loads(result.stdout)
        # An unavailable registry, unsupported report, missing metadata or a
        # scanner error is not a clean audit, even if npm exits zero.
        counts = report.get("metadata", {}).get("vulnerabilities", {})
        valid = (report.get("auditReportVersion") == 2 and not report.get("error")
                 and type(counts.get("total")) is int and counts["total"] >= 0)
        passed = valid and result.returncode == 0 and counts["total"] == 0
        results.append({"lockfile": str(lock), "exitCode": result.returncode,
                        "passed": passed, "valid": valid, "vulnerabilities": counts})
        print(f"{lock}: {'passed' if passed else 'failed'}", file=sys.stderr)
    (report_dir / "summary.json").write_text(json.dumps(results, indent=2) + "\n")
    if report_only:
        for result in results:
            if result["valid"] and not result["passed"]:
                print(f"WARNING: {result['lockfile']}: {result['vulnerabilities'].get('total')} known "
                      "vulnerabilities, recorded in the report and not failing (--report-only)", file=sys.stderr)
        return 0 if all(result["valid"] for result in results) else 1
    return 0 if all(result["passed"] for result in results) else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report-dir", type=Path, required=True)
    parser.add_argument("--report-only", action="store_true",
                        help="record findings without failing; an unreadable report still fails")
    args = parser.parse_args()
    try:
        return audit(args.report_dir, args.report_only)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print(f"npm audit incomplete: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
