#!/usr/bin/env python3
"""Build and verify release files from the pipeline's exact source checkout.

This command holds no publication credentials. The native release candidate
service owns approval, registry/release effects and their durable receipts.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import struct
import subprocess
import tarfile
import zipfile


TARGETS = (
    ("linux-x64", "linux", "amd64"),
    ("linux-arm64", "linux", "arm64"),
    ("darwin-arm64", "darwin", "arm64"),
    ("darwin-x64", "darwin", "amd64"),
    ("web", None, None),
)


def release_version(tag):
    if not re.fullmatch(r"v?[0-9]+\.[0-9]+\.[0-9]+", tag or ""):
        raise ValueError("MEMQL_VERSION must name an engine release tag")
    return tag.removeprefix("v")


def run(root, *args, capture=False, timeout=1800):
    return subprocess.run(args, cwd=root, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None,
                          timeout=timeout).stdout


def source(root, repository, sha, version):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository or ""):
        raise ValueError("MEMQL_REPOSITORY must name one GitHub repository")
    if not re.fullmatch(r"[0-9a-f]{40}", sha or ""):
        raise ValueError("MEMQL_SHA must be an exact commit")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version or ""):
        raise ValueError("MEMQL_VERSION must name the engine release")
    if (root / "VERSION").read_text().splitlines()[0].strip() != version:
        raise ValueError("release version differs from the pinned source")
    if run(root, "git", "rev-parse", "HEAD", capture=True).strip() != sha:
        raise ValueError("checkout differs from the pinned commit")
    run(root, "git", "diff", "--exit-code", "HEAD", "--")
    if run(root, "git", "rev-parse", "--is-shallow-repository", capture=True).strip() == "true":
        # Complete ancestry supplies accurate page lastUpdated dates. Blob
        # filtering avoids downloading historical file contents for a docs build.
        run(root, "git", "-c", "credential.helper=", "-c", "protocol.file.allow=never",
            "fetch", "--no-tags", "--filter=blob:none", "--unshallow", "--",
            f"https://github.com/{repository}.git", sha)
    if (run(root, "git", "rev-parse", "HEAD", capture=True).strip() != sha or
            run(root, "git", "rev-parse", "--is-shallow-repository", capture=True).strip() != "false"):
        raise ValueError("release documentation requires the exact commit and complete ancestry")


def tar_bytes(archive, name, maximum=1024 * 1024):
    item = archive.getmember(name)
    if not item.isfile() or item.size <= 0 or item.size > maximum:
        raise ValueError(f"invalid release metadata: {name}")
    return archive.extractfile(item).read(maximum + 1)


def verify_docs(path, version):
    with tarfile.open(path, "r:gz") as archive:
        manifest = json.loads(tar_bytes(archive, "./manifest.json"))
        if manifest.get("schema") != 2 or manifest.get("version") != version:
            raise ValueError("docs manifest differs from the release contract")
        if tar_bytes(archive, "./memql-docs-version").decode().strip() != version:
            raise ValueError("docs version differs from the release")
        for name in ("./llms.txt", "./llms-full.txt", "./sitemap-docs.xml"):
            item = archive.getmember(name)
            if not item.isfile() or item.size <= 0:
                raise ValueError(f"missing documentation output: {name}")


def verify_sdk(path, expected):
    with tarfile.open(path, "r:gz") as archive:
        manifest = json.loads(tar_bytes(archive, "package/package.json"))
        if (manifest.get("name"), manifest.get("version")) != (expected["name"], expected["version"]):
            raise ValueError("SDK package identity differs from its source")
        for name in ("package/dist/index.js", "package/dist/index.d.ts"):
            if not archive.getmember(name).isfile():
                raise ValueError("SDK package has no compiled entry point")


def verify_vsix(path, target, goos, goarch, version):
    with zipfile.ZipFile(path) as archive:
        manifest = json.loads(archive.read("extension/package.json"))
        if manifest.get("version") != version:
            raise ValueError("VSIX version differs from its source")
        # Packaging validates the manifest target; also inspect the actual LSP
        # executable so a mislabeled cross-build cannot pass by file name alone.
        import xml.etree.ElementTree as ET
        xml = ET.fromstring(archive.read("extension.vsixmanifest"))
        identity = xml.find("{*}Metadata/{*}Identity")
        if identity is None or identity.attrib.get("TargetPlatform") != target:
            raise ValueError("VSIX target differs from its build")
        binaries = [n for n in archive.namelist() if n.startswith("extension/bin/") and not n.endswith("/")]
        if target == "web":
            if binaries or any(n.startswith("extension/staged/") for n in archive.namelist()):
                raise ValueError("web VSIX includes native binaries or install scripts")
            if "extension/" + manifest["browser"].removeprefix("./") not in archive.namelist():
                raise ValueError("web VSIX has no browser entry point")
            return
        nodearch = {"amd64": "x64", "arm64": "arm64"}[goarch]
        expected = f"extension/bin/{goos}-{nodearch}/memql-lsp"
        if binaries != [expected] or not (archive.getinfo(expected).external_attr >> 16) & 0o111:
            raise ValueError("VSIX lacks its sole executable LSP")
        with archive.open(expected) as binary:
            header = binary.read(32)
        if goos == "linux":
            valid = (header[:6] == b"\x7fELF\x02\x01" and
                     struct.unpack_from("<H", header, 18)[0] == {"amd64": 62, "arm64": 183}[goarch])
        else:
            valid = struct.unpack_from("<II", header) == (0xFEEDFACF, {"amd64": 0x01000007, "arm64": 0x0100000C}[goarch])
        if not valid:
            raise ValueError("VSIX LSP bytes target a different operating system or architecture")


def main():
    root = Path.cwd()
    repository, sha, version = (os.environ.get(key) for key in ("MEMQL_REPOSITORY", "MEMQL_SHA", "MEMQL_VERSION"))
    version = release_version(version)
    source(root, repository, sha, version)
    output = root / ".memql-release"
    output.mkdir(exist_ok=False)  # no stale artifact can satisfy this build
    for name in ("docs", "sdk", "editors"):
        (output / name).mkdir()
    run(root, "bash", "scripts/docs/build-docs-bundle.sh", f"--version={version}", "--check")
    run(root, "bash", "scripts/docs/build-docs-bundle.sh", f"--version={version}")
    docs = output / "docs" / f"docs-{version}.tgz"
    shutil.move(root / docs.name, docs)
    verify_docs(docs, version)

    sdk = root / "sdk/ts"
    sdk_manifest = json.loads((sdk / "package.json").read_text())
    run(sdk, "npm", "ci", "--no-audit", "--no-fund")
    run(sdk, "npm", "run", "build")
    packed = json.loads(run(sdk, "npm", "pack", "--json", "--pack-destination", str(output / "sdk"), capture=True))
    if len(packed) != 1 or Path(packed[0]["filename"]).name != packed[0]["filename"]:
        raise ValueError("npm pack did not produce one local archive")
    verify_sdk(output / "sdk" / packed[0]["filename"], sdk_manifest)

    editor_version = json.loads((root / "editors/vscode/package.json").read_text())["version"]
    run(root, "bash", "scripts/identity/build-css.sh")
    for target, goos, goarch in TARGETS:
        archive = output / "editors" / f"memql-{target}.vsix"
        command = ["bash", "scripts/vscode/package.sh", f"--target={target}", f"--out={archive}"]
        if goos:
            command.extend((f"--goos={goos}", f"--goarch={goarch}"))
        run(root, *command)
        verify_vsix(archive, target, goos, goarch, editor_version)
    files = []
    for file in sorted(output.rglob("*")):
        if file.is_file():
            with file.open("rb") as body:
                digest = hashlib.file_digest(body, "sha256").hexdigest()
            files.append({"path": file.relative_to(output).as_posix(), "sha256": digest, "size": file.stat().st_size})
    (output / "manifest.json").write_text(json.dumps({"schemaVersion": 1, "repository": repository,
        "commit": sha, "engineVersion": version, "sdkVersion": sdk_manifest["version"],
        "editorVersion": editor_version, "files": files}, indent=2) + "\n")
    print(f"Verified {len(files)} release files; publication remains a separate approved effect.")


if __name__ == "__main__":
    main()
