#!/usr/bin/env python3
"""Audit each tracked npm lockfile without installing or running package scripts."""
import argparse
import json
from pathlib import Path
import subprocess
import sys


def audit(report_dir: Path) -> int:
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
                        "passed": passed, "vulnerabilities": counts})
        print(f"{lock}: {'passed' if passed else 'failed'}", file=sys.stderr)
    (report_dir / "summary.json").write_text(json.dumps(results, indent=2) + "\n")
    return 0 if all(result["passed"] for result in results) else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report-dir", type=Path, required=True)
    args = parser.parse_args()
    try:
        return audit(args.report_dir)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print(f"npm audit incomplete: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
