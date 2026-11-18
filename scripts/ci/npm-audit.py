#!/usr/bin/env python3
"""Audit each tracked npm lockfile without installing or running package scripts.

With --report-only (release runs) known vulnerabilities are recorded and
reported as warnings without failing: the release's pull request already ran
this audit, and an advisory published since must not block shipping (owner
decision, 2026-10-29). A report that cannot be read is still no audit and fails.

Exceptions (owner decision, 2026-11-17). An advisory that has no fixed release
can still fail every run with nothing anyone can upgrade to, and a failed
vulnerability stage skips the test stages behind it. npm-audit-exceptions.json,
beside this script, lists such advisories. A finding is waived only while all
three hold, and otherwise fails exactly as before:
  - an exception names that advisory id and that package;
  - the exception's `expires` date has not arrived (on that date it fails again);
  - every install path npm reports for the vulnerable package is marked
    `"dev": true` in the lockfile, so the code reaches build and test tools only
    and never anything that ships. `devOptional` does not count as dev.
A waiver is never silent: it is printed as a WARNING and recorded under
"waived" in summary.json. Remove an entry once a fixed release is adopted.
"""
import argparse
from datetime import date, datetime, timezone
import json
from pathlib import Path
import re
import subprocess
import sys

DEFAULT_EXCEPTIONS = Path(__file__).resolve().with_name("npm-audit-exceptions.json")
GHSA = re.compile(r"^GHSA(-[23456789cfghjmpqrvwx]{4}){3}$")
EXCEPTION_FIELDS = ("advisory", "package", "expires", "reason", "decided")


def load_exceptions(path: Path) -> dict:
    """Read the exceptions file into {(advisory, package): entry}.

    Any malformed entry raises ValueError, so the audit reports itself incomplete
    instead of guessing what an exception meant.
    """
    entries = json.loads(path.read_text())
    if not isinstance(entries, list):
        raise ValueError(f"{path}: expected a list of exceptions")
    exceptions = {}
    for entry in entries:
        if not isinstance(entry, dict):
            raise ValueError(f"{path}: every exception must be an object")
        missing = [field for field in EXCEPTION_FIELDS
                   if not isinstance(entry.get(field), str) or not entry[field].strip()]
        if missing:
            raise ValueError(f"{path}: exception {entry.get('advisory')!r} lacks {', '.join(missing)}")
        if not GHSA.match(entry["advisory"]):
            raise ValueError(f"{path}: {entry['advisory']!r} is not a GitHub advisory id")
        exceptions[(entry["advisory"], entry["package"])] = dict(entry, expires=date.fromisoformat(entry["expires"]))
    return exceptions


def judge(report: dict, packages: dict, exceptions: dict, today: date) -> tuple:
    """Split one report's advisories into (waived, refused) descriptions.

    Only `via` entries that are objects are advisories; a string names another
    package whose own advisory is judged where it appears.
    """
    waived, refused = [], []
    vulnerabilities = report.get("vulnerabilities") or {}
    for name, vulnerability in vulnerabilities.items():
        for via in vulnerability.get("via") or []:
            if not isinstance(via, dict):
                continue
            advisory = str(via.get("url", "")).rstrip("/").rsplit("/", 1)[-1]
            package = via.get("name") or name
            nodes = (vulnerabilities.get(package) or vulnerability).get("nodes") or []
            shipped = [node for node in nodes if (packages.get(node) or {}).get("dev") is not True]
            exception = exceptions.get((advisory, package))
            label = f"{advisory} in {package}"
            if exception is None:
                refused.append(f"{label}: no exception")
            elif today >= exception["expires"]:
                refused.append(f"{label}: its exception expired on {exception['expires']}")
            elif not nodes or shipped:
                refused.append(f"{label}: reaches a shipped dependency ({', '.join(shipped) or 'no install path reported'})")
            else:
                waived.append(f"{label} until {exception['expires']}: {exception['reason']}")
    return waived, refused


def audit(report_dir: Path, report_only: bool = False, exceptions_path: Path = DEFAULT_EXCEPTIONS,
          today: date = None) -> int:
    today = today or datetime.now(timezone.utc).date()
    exceptions = load_exceptions(exceptions_path) if exceptions_path.exists() else {}
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
        waived, refused = [], []
        # npm audit exits 1 when it reports findings; any other code is not a
        # report a waiver can be judged against.
        if valid and counts["total"] > 0 and result.returncode in (0, 1):
            packages = json.loads(lock.read_text()).get("packages") or {}
            waived, refused = judge(report, packages, exceptions, today)
            passed = bool(waived) and not refused
        for line in waived:
            print(f"WARNING: {lock}: waived {line} ({exceptions_path.name})", file=sys.stderr)
        for line in refused:
            print(f"{lock}: {line}", file=sys.stderr)
        results.append({"lockfile": str(lock), "exitCode": result.returncode,
                        "passed": passed, "valid": valid, "vulnerabilities": counts,
                        "waived": waived})
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
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--report-dir", type=Path, required=True)
    parser.add_argument("--report-only", action="store_true",
                        help="record findings without failing; an unreadable report still fails")
    parser.add_argument("--exceptions", type=Path, default=DEFAULT_EXCEPTIONS,
                        help="the exceptions file (default: npm-audit-exceptions.json beside this script)")
    parser.add_argument("--today", type=date.fromisoformat, default=None,
                        help="the date exceptions expire against, YYYY-MM-DD (default: today in UTC)")
    args = parser.parse_args()
    try:
        return audit(args.report_dir, args.report_only, args.exceptions, args.today)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print(f"npm audit incomplete: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
