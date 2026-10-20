#!/usr/bin/env python3
"""Run the full upstream repository audit, retaining JSON and native SARIF.

Scorecard omits HEAD-only checks when --commit is supplied. Audit HEAD and
refuse publication evidence if it differs from the pipeline's pinned source.
The pipeline owns credentials and scheduling; this command never publishes.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import selectors
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request

VERSION = "5.5.0"  # The version used by the retiring scorecard-action v2.4.4.
ASSETS = {
    "x86_64": ("amd64", "83b90a05c1540ef1390db1cd5711e5fd04be9c1d8537fb84d39d02092d6a8dff"),
    "aarch64": ("arm64", "3ce59d20c1d53e540c4a14e0da1e0d96b3b294e8ddc96a3c5a7b8a637b32991e"),
}
MAX_ARCHIVE = 30 * 1024 * 1024
MAX_REPORT = 20 * 1024 * 1024
# Preserve the retiring action's policy, including its disabled checks.
# Source: ossf/scorecard-action@2d1146689b8cda280b9bc96326124645441f03bc,
# policies/template.yml, Apache-2.0 (copyright notice retained in the file).
POLICY = Path(__file__).with_name("scorecard-policy.yml")
POLICY_SHA256 = "6e75dcc0df989d333492c0e1f6e484fc5b4d26ab768333ddcd0e3788539689b0"
# The CLI analyzes all 18 checks; policy enforcement excludes two. The
# nine checks supported at historical commits do not constitute a full audit.
CHECKS = {
    "Binary-Artifacts", "Branch-Protection", "CI-Tests", "CII-Best-Practices",
    "Code-Review", "Dangerous-Workflow", "Dependency-Update-Tool", "Fuzzing",
    "License", "Maintained", "Pinned-Dependencies", "SAST", "Security-Policy",
    "Packaging", "Token-Permissions", "Vulnerabilities", "Contributors", "Signed-Releases",
}


def bounded_json(path):
    if not path.is_file() or not 0 < path.stat().st_size <= MAX_REPORT:
        raise ValueError("missing or oversized audit report")
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError("audit report must be one JSON object")
    return value


def validate_results(result, repository, sha):
    if result.get("repo") != {"name": "github.com/" + repository, "commit": sha}:
        raise ValueError("repository HEAD changed or the audit names another source; request a fresh audit")
    if result.get("scorecard", {}).get("version") != "v" + VERSION:
        raise ValueError("unexpected Scorecard version")
    checks = result.get("checks")
    if (not isinstance(checks, list) or not all(isinstance(c, dict) for c in checks)
            or {c.get("name") for c in checks} != CHECKS or len(checks) != len(CHECKS)):
        raise ValueError("Scorecard did not execute the complete policy check set")
    for check in checks:
        if type(check.get("score")) is not int or not -1 <= check["score"] <= 10:
            raise ValueError("invalid Scorecard check result")
        if not isinstance(check.get("reason"), str):
            raise ValueError("Scorecard omitted the check's explanation")
        if "internal error" in check["reason"].lower():
            raise ValueError("Scorecard could not complete " + check["name"])
    return [c["name"] for c in checks if c["score"] == -1]


def validate_sarif(report):
    if report.get("version") != "2.1.0" or not isinstance(report.get("runs"), list) or not report["runs"]:
        raise ValueError("Scorecard produced no SARIF runs")
    for run in report["runs"]:
        driver = run.get("tool", {}).get("driver", {})
        if driver.get("name") != "Scorecard" or driver.get("semanticVersion") != "v" + VERSION:
            raise ValueError("SARIF names a different analyzer")
        if not isinstance(run.get("results"), list):
            raise ValueError("SARIF omitted its findings")
    return sum(len(run["results"]) for run in report["runs"])


def install_binary(directory):
    if platform.system() != "Linux" or platform.machine() not in ASSETS:
        raise ValueError("the pinned Scorecard bootstrap requires Linux AMD64 or ARM64")
    arch, digest = ASSETS[platform.machine()]
    url = f"https://github.com/ossf/scorecard/releases/download/v{VERSION}/scorecard_{VERSION}_linux_{arch}.tar.gz"
    archive = directory / "scorecard.tar.gz"
    with urllib.request.urlopen(url, timeout=60) as response, archive.open("xb") as output:
        size, sha = 0, hashlib.sha256()
        while block := response.read(1024 * 1024):
            size += len(block)
            if size > MAX_ARCHIVE:
                raise ValueError("Scorecard archive exceeds its byte bound")
            sha.update(block)
            output.write(block)
    if sha.hexdigest() != digest:
        raise ValueError("Scorecard archive differs from the pinned SHA-256")
    binary = directory / "scorecard"
    with tarfile.open(archive) as tar:
        members = [m for m in tar.getmembers() if m.name == "scorecard"]
        if len(members) != 1 or not members[0].isfile() or not 0 < members[0].size <= 200 * 1024 * 1024:
            raise ValueError("Scorecard archive has no sole bounded regular executable")
        with binary.open("xb") as output:
            output.write(tar.extractfile(members[0]).read())
    binary.chmod(0o700)
    archive.unlink()
    return binary


def head(repository, token):
    # No authenticated redirects: the fixed GitHub API is the sole recipient.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None
    request = urllib.request.Request(
        "https://api.github.com/repos/" + repository + "/commits/HEAD",
        headers={"Authorization": "Bearer " + token, "Accept": "application/vnd.github+json"},
    )
    with urllib.request.build_opener(NoRedirect).open(request, timeout=30) as response:
        value = response.read(1024 * 1024 + 1)
    if len(value) > 1024 * 1024:
        raise ValueError("repository-head response exceeded its bound")
    return json.loads(value).get("sha")


def run_bounded(command, env, timeout=600, maximum=MAX_REPORT):
    """Bound both pipes while they are produced, and retire child processes."""
    with subprocess.Popen(command, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                          start_new_session=True) as process:
        streams = {process.stdout: bytearray(), process.stderr: bytearray()}
        deadline = time.monotonic() + timeout
        try:
            with selectors.DefaultSelector() as selector:
                for stream in streams:
                    selector.register(stream, selectors.EVENT_READ)
                while selector.get_map():
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise ValueError("Scorecard analysis exceeded its execution bound")
                    for key, _ in selector.select(min(remaining, 1)):
                        data = os.read(key.fileobj.fileno(), 65536)
                        if not data:
                            selector.unregister(key.fileobj)
                            continue
                        output = streams[key.fileobj]
                        if len(output) + len(data) > maximum:
                            raise ValueError("Scorecard output exceeded its byte bound")
                        output.extend(data)
            code = process.wait(timeout=max(0.01, deadline - time.monotonic()))
            return code, bytes(streams[process.stdout]), bytes(streams[process.stderr])
        finally:
            # Scorecard may have started git children; retire this command's
            # isolated group even when the parent exits before closing a pipe.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--scorecard", type=Path, help="use a preinstalled pinned CLI")
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)
    if any(args.output_dir.iterdir()):
        parser.error("output directory must be empty; each attempt needs fresh evidence")
    summary = {"version": VERSION, "status": "failed", "scope": "full repository audit"}
    token = os.environ.get("MEMQL_GITHUB_READ_TOKEN", "")
    try:
        repo, sha = os.environ.get("MEMQL_REPOSITORY", ""), os.environ.get("MEMQL_SHA", "")
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) or not re.fullmatch(r"[0-9a-f]{40}", sha):
            raise ValueError("audit requires the repository and exact pipeline commit")
        summary.update(repository=repo, sourceCommit=sha)
        if not token:
            raise ValueError("MEMQL_GITHUB_READ_TOKEN is supplied by githubReadToken: true")
        if hashlib.sha256(POLICY.read_bytes()).hexdigest() != POLICY_SHA256:
            raise ValueError("Scorecard policy differs from the reviewed action policy")
        summary["policySHA256"] = POLICY_SHA256
        if head(repo, token) != sha:
            raise ValueError("repository HEAD moved while this audit waited; request a fresh audit")
        env = dict(os.environ, GITHUB_AUTH_TOKEN=token, ENABLE_SARIF="1")
        for key in ("GITHUB_TOKEN", "GH_TOKEN", "GH_AUTH_TOKEN", "GITHUB_APP_KEY_PATH", "GITHUB_APP_ID", "GITHUB_APP_INSTALLATION_ID"):
            env.pop(key, None)
        with tempfile.TemporaryDirectory(prefix="memql-scorecard-") as temporary:
            scratch = Path(temporary)
            binary = args.scorecard.resolve() if args.scorecard else install_binary(scratch)
            for form, filename in (("json", "results.json"), ("sarif", "results.sarif")):
                code, stdout, stderr = run_bounded(
                    [str(binary), "--repo=github.com/" + repo, "--format=" + form,
                     "--policy=" + str(POLICY.resolve()), "--show-details", "--verbosity=warn"],
                    env,
                )
                (args.output_dir / filename).write_bytes(stdout.replace(token.encode(), b"[redacted]"))
                (args.output_dir / (form + ".log")).write_bytes(stderr.replace(token.encode(), b"[redacted]"))
                if code:
                    raise ValueError("Scorecard analysis failed; see retained " + form + ".log")
                if form == "json":
                    summary["unscoredChecks"] = validate_results(bounded_json(args.output_dir / filename), repo, sha)
                if head(repo, token) != sha:
                    raise ValueError("repository HEAD changed during analysis; request a fresh audit")
        report = args.output_dir / "results.sarif"
        summary.update(status="analyzed", checkCount=len(CHECKS), findings=validate_sarif(bounded_json(report)),
                       sarifSHA256=hashlib.sha256(report.read_bytes()).hexdigest())
    except (OSError, ValueError, subprocess.SubprocessError, tarfile.TarError) as exc:
        summary["error"] = str(exc).replace(token, "[redacted]") if token else str(exc)
        print(summary["error"], file=sys.stderr)
    (args.output_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary))
    return 0 if summary["status"] == "analyzed" else 1


if __name__ == "__main__":
    sys.exit(main())
