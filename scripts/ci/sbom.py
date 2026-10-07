#!/usr/bin/env python3
"""Inventory every tracked Go module and npm lockfile as CycloneDX SBOMs."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import xml.etree.ElementTree as ET


def tracked(patterns):
    output = subprocess.check_output(["git", "ls-files", "-z", "--", *patterns], text=True)
    return sorted({Path(path) for path in output.split("\0") if path})


def generate(command, destination, cwd, env):
    result = subprocess.run(command, cwd=cwd, env=env, text=True, capture_output=True, timeout=600)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(result.stdout)
    destination.with_suffix(".stderr.txt").write_text(result.stderr)
    if result.returncode:
        raise RuntimeError(f"SBOM failed for {cwd}: exit {result.returncode}; see {destination}")
    report = json.loads(result.stdout)
    if (report.get("bomFormat") != "CycloneDX" or not report.get("specVersion")
            or not report.get("metadata", {}).get("component", {}).get("name")
            or not isinstance(report.get("components", []), list)):
        raise RuntimeError(f"incomplete CycloneDX report for {cwd}")
    return {"path": str(destination), "components": len(report.get("components", [])),
            "sha256": hashlib.sha256(destination.read_bytes()).hexdigest()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--cyclonedx", default="cyclonedx-gomod")
    parser.add_argument("--syft", default="syft")
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)
    if any(args.output_dir.iterdir()):
        parser.error("output directory must be empty; each attempt needs fresh evidence")
    summary = {"status": "failed", "reports": []}
    try:
        modules = tracked(["go.mod", "**/go.mod"])
        locks = tracked(["package-lock.json", "**/package-lock.json"])
        if not modules or not locks:
            raise RuntimeError("both Go module and npm lockfile inventories must be nonempty")
        summary["sourceCommit"] = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
        summary["goModules"] = len(modules)
        summary["npmLockfiles"] = len(locks)
        # The engine is built as a Go workspace. Generate its resolved graph
        # once, then verify that every tracked module is represented; repeating
        # `mod` per directory under go.work would duplicate the same graph.
        module_names = set()
        for module in modules:
            metadata = json.loads(subprocess.check_output(["go", "mod", "edit", "-json", str(module)], text=True))
            module_names.add(metadata["Module"]["Path"])
        destination = args.output_dir / "go" / "bom.cdx.json"
        report = generate([args.cyclonedx, "mod", "-licenses", "-json"], destination, ".", os.environ)
        bom = json.loads(destination.read_text())
        represented = {component["name"] for component in bom.get("components", [])}
        represented.add(bom["metadata"]["component"]["name"])
        missing = sorted(module_names - represented)
        if missing:
            raise RuntimeError(f"workspace SBOM omitted tracked modules: {missing}")
        summary["reports"].append(dict(report, source="go.work"))
        print(f"inventoried {len(modules)} Go workspace modules", file=sys.stderr)
        # Preserve the previous release lane's root XML deliverable as well.
        xml = subprocess.run([args.cyclonedx, "mod", "-licenses"], capture_output=True, timeout=600)
        xml_path = args.output_dir / "go" / "bom.cdx.xml"
        xml_path.write_bytes(xml.stdout)
        (args.output_dir / "go" / "xml.stderr.txt").write_bytes(xml.stderr)
        if xml.returncode or ET.fromstring(xml.stdout).tag.split("}")[-1] != "bom":
            raise RuntimeError("root XML SBOM generation failed")
        summary["reports"].append({"path": str(xml_path), "source": "go.work",
                                   "sha256": hashlib.sha256(xml.stdout).hexdigest()})
        for lock in locks:
            destination = args.output_dir / "npm" / lock.parent / "bom.cdx.json"
            report = generate([args.syft, "scan", "file:" + str(lock.resolve()),
                               "--override-default-catalogers", "javascript-lock-cataloger",
                               "--parallelism=1", "-o", "cyclonedx-json"],
                              destination, ".", dict(os.environ, SYFT_CHECK_FOR_APP_UPDATE="false",
                                                          SYFT_JAVASCRIPT_INCLUDE_DEV_DEPENDENCIES="true"))
            lock_data = json.loads(lock.read_text())
            if lock_data.get("lockfileVersion") not in (2, 3) or not isinstance(lock_data.get("packages"), dict):
                raise RuntimeError(f"unsupported npm lockfile inventory: {lock}")
            expected = {(pkg.get("name", path.rsplit("node_modules/", 1)[-1]), pkg["version"])
                        for path, pkg in lock_data["packages"].items()
                        if "node_modules/" in path and not pkg.get("link") and pkg.get("version")}
            components = json.loads(destination.read_text()).get("components", [])
            actual = {((component.get("group", "") + "/" if component.get("group") else "") + component["name"],
                       component.get("version")) for component in components}
            if expected - actual:
                raise RuntimeError(f"npm SBOM omitted locked packages from {lock}: {sorted(expected - actual)}")
            summary["reports"].append(dict(report, source=str(lock), lockedPackages=len(expected)))
            print(f"inventoried {lock}", file=sys.stderr)
        summary["status"] = "complete"
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError, ET.ParseError) as exc:
        summary["error"] = str(exc)
        print(str(exc), file=sys.stderr)
    (args.output_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    return 0 if summary["status"] == "complete" else 1


if __name__ == "__main__":
    sys.exit(main())
