#!/usr/bin/env python3
"""Run real editor hosts and retain each lane's result without an Actions runner.

Linux requires the toolchain's display/browser libraries. macOS runs on an
authorized native Cockpit worker. This never installs system packages, publishes
an extension, or treats cross-compilation as host execution. The pipeline worker
owns the enclosing deadline and cancellation of this process group.
"""
import argparse
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import time


def native_platform():
    systems = {"Linux": "linux", "Darwin": "darwin"}
    arches = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}
    try:
        return systems[platform.system()] + "/" + arches[platform.machine()]
    except KeyError as exc:
        raise ValueError("editor host tests require native Linux or macOS on amd64 or arm64") from exc


def lanes(suite, host_platform):
    desktop = ["xvfb-run", "-a", "-s", "-screen 0 1280x1024x24"] if host_platform.startswith("linux/") else []
    selected = []
    if suite in ("all", "core"):
        for name, version in (("minimum", "1.91.0"), ("stable", "stable")):
            selected.append(("core-" + name, version, "editors/vscode", desktop + ["npm", "run", "test:host"]))
    if suite in ("all", "productivity"):
        selected.append(("productivity-web", "stable", ".", ["node", "editors/productivity/test/runner.cjs"]))
        for name, version in (("minimum", "1.91.0"), ("stable", "stable")):
            selected.append(("productivity-" + name, version, ".", desktop + ["node", "editors/productivity/test/runner.cjs", "--desktop"]))
    if not selected:
        raise ValueError("no editor host lanes selected")
    return selected


def execute(command, cwd, env, log):
    # Keep the worker's process group so its cancellation also reaches Electron,
    # Chromium and Xvfb. A detached session would outlive the owning work step.
    with log.open("xb") as output:
        return subprocess.run(command, cwd=cwd, env=env, stdout=output, stderr=subprocess.STDOUT, check=False).returncode


def run(root, output, host_platform, suite):
    actual = native_platform()
    if actual != host_platform:
        raise ValueError(f"requested {host_platform}, but this worker is {actual}; host tests cannot be cross-compiled")
    selected = lanes(suite, host_platform)
    env = dict(os.environ, CI="true", GOFLAGS="-p=1", GOMEMLIMIT="512MiB", GOGC="50", CGO_ENABLED="0")
    # The caller selects only the declared standard hosts. Ambient developer
    # overrides must not replace them or attach a test to a personal cluster.
    for name in ("MEMQL_VSCODE_EXECUTABLE", "MEMQL_HOST_SMOKE_CLUSTERS_FILE", "MEMQL_HOST_SMOKE_CLUSTER"):
        env.pop(name, None)
    summary = {"status": "failed", "platform": actual, "suite": suite, "lanes": []}
    summary["sourceCommit"] = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
    summary["trackedChanges"] = bool(subprocess.check_output(["git", "status", "--porcelain", "-uno"], cwd=root, text=True))
    node_os, arch = actual.split("/")
    env.update(GOOS=node_os, GOARCH=arch)
    node_arch = "x64" if arch == "amd64" else "arm64"
    binary = root / "editors/vscode/bin" / f"{node_os}-{node_arch}" / "memql-lsp"
    binary.parent.mkdir(parents=True, exist_ok=True)
    prepare = [
        ("workspace", ".", ["bash", "scripts/vscode/deps.sh"]),
        ("core-install", "editors/vscode", ["npm", "ci", "--no-audit", "--no-fund"]),
        ("language-server", ".", ["go", "build", "-o", str(binary), "./cmd/memql-lsp"]),
    ]
    if suite in ("all", "productivity"):
        prepare += [
            ("productivity-install", "editors/productivity", ["npm", "ci", "--no-audit", "--no-fund"]),
            ("core-build", "editors/vscode", ["npm", "run", "compile"]),
            ("productivity-build", "editors/productivity", ["npm", "run", "compile"]),
            ("browser-install", "editors/vscode", ["npx", "--no-install", "playwright", "install", "chromium"]),
        ]
    try:
        for name, cwd, command in prepare:
            print(f"Preparing editor hosts: {name}", flush=True)
            code = execute(command, root / cwd, env, output / (name + ".log"))
            if code:
                summary["error"] = f"{name} exited {code}; see {name}.log"
                return 1
        for name, version, cwd, command in selected:
            print(f"Running {name} on {actual}", flush=True)
            started = time.monotonic()
            code = execute(command, root / cwd, dict(env, MEMQL_VSCODE_VERSION=version), output / (name + ".log"))
            summary["lanes"].append({"name": name, "requestedVersion": version, "exitCode": code,
                                     "status": "passed" if code == 0 else "failed", "seconds": round(time.monotonic() - started, 3)})
            if code and name.startswith("productivity-"):
                logs = root / "editors/productivity/dist-test/host-logs"
                if logs.is_dir():
                    shutil.copytree(logs, output / (name + "-diagnostics"))
        passed = len(summary["lanes"]) == len(selected) and all(lane["status"] == "passed" for lane in summary["lanes"])
        if passed:
            summary["status"] = "passed"
        return 0 if passed else 1
    except OSError as exc:
        summary["error"] = str(exc)
        return 1
    finally:
        (output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        print(f"Editor host tests {summary['status']}; results in {output}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--platform", required=True, choices=("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"))
    parser.add_argument("--suite", choices=("all", "core", "productivity"), default="all")
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    output = args.output_dir.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        parser.error("output directory must be empty; each attempt needs fresh evidence")
    try:
        return run(root, output, args.platform, args.suite)
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        (output / "summary.json").write_text(json.dumps({"status": "failed", "error": str(exc)}, indent=2) + "\n")
        print(str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
