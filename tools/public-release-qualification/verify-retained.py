#!/usr/bin/env python3
"""Static, offline review of candidate-assets downloads; never execute a product.

The default prints a plan. --execute reads exactly the downloaded 41 assets,
checksums, SBOMs, Go metadata and Mac signatures. It does not authenticate, fetch,
approve, publish, enroll, launch an app or change permissions.
"""
import argparse
import json
import re
import shutil
import sys
from pathlib import Path
import qualify as q


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--downloads", required=True, type=Path)
    p.add_argument("--tag", required=True)
    p.add_argument("--source-commit", required=True)
    p.add_argument("--output", required=True, type=Path)
    p.add_argument("--execute", action="store_true")
    args = p.parse_args()
    q.require(re.fullmatch(r"cockpit/v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", args.tag), "Exact stable Cockpit tag required")
    q.require(re.fullmatch(r"[0-9a-f]{40}", args.source_commit), "Exact source commit required")
    if not args.execute:
        print(json.dumps({"planned": True, "assetCount": 41, "network": False, "productExecution": False,
                          "checks": ["candidate byte digests and sizes", "9 checksum files", "16 SBOMs", "8 raw binary Go identities", "Mac app workers, bundle IDs and static signatures"]}))
        return
    root, assets = args.output.resolve(), args.downloads.resolve() / "assets"
    q.require(not root.exists(), "Static review output must be new")
    version = args.tag.removeprefix("cockpit/v")
    expected = q.expected_assets(version)
    candidate = args.downloads.resolve() / "candidate.json"
    digests = q.candidate_digests(candidate, version, args.source_commit, expected)
    record = q.payload(json.loads(candidate.read_text()))
    sizes = {a["name"]: a["size"] for a in record["manifest"]["components"][0]["artifacts"]}
    q.require({a.name for a in assets.iterdir()} == set(expected.values()), "Downloaded asset inventory differs from candidate")
    root.mkdir()
    report = {"tag": args.tag, "sourceCommit": args.source_commit, "candidateId": record["candidateId"],
              "network": False, "productExecution": False, "assets": []}
    try:
        for artifact, name in expected.items():
            path = assets / name
            q.require(path.is_file() and not path.is_symlink(), "Asset must be a regular local file")
            q.require(path.stat().st_size == sizes[artifact] and q.sha(path) == digests[artifact], "Retained asset bytes differ from candidate: " + name)
            report["assets"].append({"name": name, "size": sizes[artifact], "sha256": digests[artifact]})
        q.verify_evidence(assets, expected, version)
        report["checksumsAndSBOMs"] = "all verified"
        go = shutil.which("go")
        q.require(go is not None, "Go metadata reader required")
        report["buildMetadata"] = {}
        for target in q.PLATFORMS:
            for stem in ("memql", "memql-computeruse"):
                name = f"{stem}-{target}"
                build = q.command([go, "version", "-m", str(assets / name)], timeout=30)
                q.verify_go_metadata(build, stem, target, version, args.source_commit)
                report["buildMetadata"][name] = build
        report["macSignatures"] = q.signature_inspection(root, assets, version, args.source_commit, go)
        report["notExecuted"] = ["all product executables on every platform", "installers", "worker", "apps", "permission or service commands"]
        report["ok"] = True
    except Exception as exc:
        report["ok"], report["error"] = False, str(exc)
        raise
    finally:
        (root / "retained-qualification.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"ok": True, "report": str(root / "retained-qualification.json"), "assetsVerified": 41}))


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(json.dumps({"ok": False, "error": str(exc)}))
        sys.exit(5)
