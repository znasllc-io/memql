#!/usr/bin/env python3
"""Run the repository's CodeQL suite without an Actions runner.

The caller owns scheduling and any SARIF upload. This command only analyzes its
checkout and retains reports. Public GitHub repositories may use the CodeQL CLI;
private repositories need a separate license decision before invoking it.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.request

import yaml

VERSION = "2.27.1"
BUNDLES = {
    "x86_64": ("linux64", "1d380f79896ededc654c7b21fafb3360136f1aeb678ad4df4df9af3910c6b815"),
    "aarch64": ("linux-arm64", "5f05cc0793971e48824d00e570afe6144a27a325360b43bd823d755aed919817"),
}


def install_bundle(destination):
    if platform.system() != "Linux" or platform.machine() not in BUNDLES:
        raise RuntimeError("the pinned CodeQL bootstrap requires Linux amd64 or arm64")
    asset, digest = BUNDLES[platform.machine()]
    url = f"https://github.com/github/codeql-action/releases/download/codeql-bundle-v{VERSION}/codeql-bundle-{asset}.tar.gz"
    archive = destination / "bundle.tar.gz"
    sha = hashlib.sha256()
    with urllib.request.urlopen(url, timeout=60) as response, archive.open("xb") as output:
        while block := response.read(1024 * 1024):
            sha.update(block)
            output.write(block)
    if sha.hexdigest() != digest:
        raise RuntimeError("CodeQL bundle SHA-256 does not match its pinned release")
    extract_bundle(archive, destination)
    archive.unlink()
    return destination / "codeql" / "codeql"


def extract_bundle(archive, destination):
    """Extract files before links; no archive path may traverse a created link.

    The toolchain's Python predates tarfile's data filter. Do not use unfiltered
    extractall: validating link targets before extraction misses link chains
    created by earlier archive entries. Hash verification is a separate check.
    """
    destination = destination.resolve()
    links = []
    names = set()
    with tarfile.open(archive) as bundle:
        for member in bundle:
            path = Path(member.name)
            if path.is_absolute() or ".." in path.parts:
                raise RuntimeError("unsafe path in CodeQL bundle")
            target = (destination / path).resolve()
            if not target.is_relative_to(destination):
                raise RuntimeError("archive member leaves the extraction directory")
            if target in names:
                raise RuntimeError("duplicate path in CodeQL bundle")
            names.add(target)
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            elif member.isfile():
                target.parent.mkdir(parents=True, exist_ok=True)
                with bundle.extractfile(member) as source, target.open("xb") as output:
                    shutil.copyfileobj(source, output)
                target.chmod(member.mode & 0o777)
            elif member.issym() or member.islnk():
                links.append((member, target))
            else:
                raise RuntimeError("unsupported special file in CodeQL bundle")
        for member, target in links:
            parent = target.parent.resolve()
            base = parent if member.issym() else destination
            link = Path(member.linkname)
            referent = (base / link).resolve()
            if (link.is_absolute() or not parent.is_relative_to(destination)
                    or not referent.is_relative_to(destination)):
                raise RuntimeError("unsafe link in CodeQL bundle")
            parent.mkdir(parents=True, exist_ok=True)
            if member.issym():
                target.symlink_to(member.linkname)
            else:
                os.link(referent, target)


def run_command(command, log, env):
    with log.open("w") as output:
        result = subprocess.run(command, stdout=output, stderr=subprocess.STDOUT, env=env, check=False)
    if result.returncode:
        raise RuntimeError(f"{command[1:3]} failed with exit {result.returncode}; see {log}")


def validate_sarif(path):
    report = json.loads(path.read_text())
    if report.get("version") != "2.1.0" or not isinstance(report.get("runs"), list) or not report["runs"]:
        raise RuntimeError("CodeQL produced no valid SARIF run")
    findings = 0
    for run in report["runs"]:
        invocations = run.get("invocations", [])
        if not invocations or any(inv.get("executionSuccessful") is not True for inv in invocations):
            raise RuntimeError("CodeQL SARIF does not confirm successful analysis")
        if not isinstance(run.get("results"), list):
            raise RuntimeError("CodeQL SARIF has no results array")
        findings += len(run["results"])
    return findings


def positive_int(value):
    number = int(value)
    if number < 1:
        raise argparse.ArgumentTypeError("must be positive")
    return number


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--language", choices=("go", "javascript-typescript", "python"), required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--threads", type=positive_int, default=2)
    parser.add_argument("--ram", type=positive_int, default=4096, help="CodeQL memory budget in MiB")
    parser.add_argument("--codeql", type=Path, help="use a preinstalled CLI instead of the pinned bootstrap")
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)
    if any(args.output_dir.iterdir()):
        parser.error("output directory must be empty; each attempt needs fresh evidence")
    summary = {"language": args.language, "status": "failed", "findings": None, "version": VERSION}
    try:
        summary["sourceCommit"] = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
        summary["trackedChanges"] = bool(subprocess.check_output(["git", "status", "--porcelain", "-uno"], text=True))
        # Exclusive names prevent stale SARIF from a previous attempt becoming
        # this attempt's evidence. The durable runner owns outer retention.
        with tempfile.TemporaryDirectory(prefix="memql-codeql-") as scratch:
            scratch = Path(scratch)
            codeql = args.codeql.resolve() if args.codeql else install_bundle(scratch)
            env = dict(os.environ, GOFLAGS="-p=1", GOGC="50", GOMEMLIMIT="512MiB")
            run_command([str(codeql), "version", "--format=json"], args.output_dir / "version.json", env)
            installed = json.loads((args.output_dir / "version.json").read_text())
            if installed.get("version") != VERSION:
                raise RuntimeError(f"expected CodeQL {VERSION}, got {installed.get('version')}")
            config = yaml.safe_load(Path(".github/codeql/codeql-config.yml").read_text())
            config["queries"] = [{"uses": "security-extended"}]
            config_path = scratch / "config.yml"
            config_path.write_text(yaml.safe_dump(config))
            database = scratch / "database"
            resources = [f"--threads={args.threads}", f"--ram={args.ram}"]
            create = [str(codeql), "database", "create", str(database), f"--language={args.language}",
                      f"--source-root={Path.cwd()}", f"--codescanning-config={config_path}", *resources]
            if args.language == "go":
                run_command(["bash", "scripts/identity/build-css.sh"], args.output_dir / "build-css.log", env)
                create += ["--command=go build github.com/znasllc-io/memql/..."]
            else:
                create += ["--build-mode=none"]
            run_command(create, args.output_dir / "extract.log", env)
            sarif = scratch / "results.sarif"
            run_command([str(codeql), "database", "analyze", str(database), "--format=sarifv2.1.0",
                         f"--output={sarif}", f"--sarif-category=/language:{args.language}",
                         "--max-disk-cache=2048", *resources], args.output_dir / "analyze.log", env)
            findings = validate_sarif(sarif)
            shutil.copyfile(sarif, args.output_dir / "results.sarif")
            # Existing CodeQL lanes report findings for triage; tool failures
            # fail the lane. Preserve that policy, and state findings explicitly.
            summary.update(status="analyzed", findings=findings, sarifSHA256=hashlib.sha256(sarif.read_bytes()).hexdigest())
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError, tarfile.TarError, yaml.YAMLError) as exc:
        summary["error"] = str(exc)
        print(str(exc), file=sys.stderr)
    (args.output_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary))
    return 0 if summary["status"] == "analyzed" else 1


if __name__ == "__main__":
    sys.exit(main())
