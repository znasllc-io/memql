#!/usr/bin/env python3
"""Prepared public Cockpit qualification. Default is a no-network/no-install plan.

--mode=verify --execute downloads only public bytes and reads Go metadata.
--mode=install --execute additionally executes only the headless macOS version
command and standalone installer inside new HOME/BIN_DIR paths. No enrollment,
worker, service, permission, policy, app bundle or credential action is allowed.
"""
import argparse
import hashlib
import json
import os
import platform
import plistlib
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
from pathlib import Path

REPO = "znasllc-io/memql"
API = f"https://api.github.com/repos/{REPO}"
RAW = f"https://raw.githubusercontent.com/{REPO}"
PLATFORMS = ("darwin-arm64", "darwin-amd64", "linux-amd64", "linux-arm64")
PUBLIC_FILES = ("cockpit/install.sh", "cockpit/scripts/install/install-mac.sh", "cockpit/scripts/install/install-linux.sh", "cockpit/scripts/install/lib.sh", "cockpit/scripts/install/release-version.py")
ALLOWED_HOSTS = {"api.github.com", "github.com", "raw.githubusercontent.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com"}


def require(test, message):
    if not test:
        raise ValueError(message)


def sha(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


class PublicRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        checked_url(newurl)
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def checked_url(url):
    u = urllib.parse.urlparse(url)
    require(u.scheme == "https" and u.hostname in ALLOWED_HOSTS and not u.username and not u.password, "Non-public/unexpected download origin refused")
    return url


def fetch(url, path, limit=512 * 1024 * 1024):
    checked_url(url)
    opener = urllib.request.build_opener(PublicRedirect())
    req = urllib.request.Request(url, headers={"User-Agent": "MemQL-public-release-qualification", "Accept": "application/vnd.github+json" if url.startswith(API) else "*/*"})
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".partial")
    count = 0
    with opener.open(req, timeout=60) as r, temporary.open("wb") as out:
        while True:
            chunk = r.read(1024 * 1024)
            if not chunk:
                break
            count += len(chunk)
            require(count <= limit, "Public download exceeds bounded size")
            out.write(chunk)
    temporary.replace(path)
    return count


def expected_assets(version):
    out = {}
    for target in PLATFORMS:
        out[f"headless-{target}"] = f"memql-{version}-{target}.tar.gz"
        out[f"headless-{target}-binary"] = f"memql-{target}"
        out[f"computeruse-{target}"] = f"memql-computeruse-{target}"
        out[f"computeruse-{target}-checksum"] = f"memql-computeruse-{target}.sha256"
        for flavour, stem in (("headless", "memql"), ("computeruse", "memql-computeruse")):
            for ext in ("json", "xml"):
                out[f"{flavour}-{target}-sbom-{ext}"] = f"{stem}-{target}.cdx.{ext}"
        if target.startswith("darwin"):
            for kind in ("app", "menubar"):
                out[f"{kind}-{target}"] = f"memql-{kind}-{target}.tar.gz"
                out[f"{kind}-{target}-checksum"] = f"memql-{kind}-{target}.tar.gz.sha256"
    out["headless-checksums"] = f"memql-{version}-SHA256SUMS"
    require(len(out) == 41, "Internal expected asset set is incomplete")
    return out


def payload(value):
    if "manifest" in value:
        return value
    value = value.get("result", value)
    rows = value.get("data", [])
    require(len(rows) == 1 and len(rows[0]) == 1, "Ambiguous native candidate evidence")
    return next(iter(rows[0].values()))["payload"]


def candidate_digests(path, version, commit, expected):
    record = payload(json.loads(path.read_text()))
    components = record["manifest"]["components"]
    require(len(components) == 1, "Candidate must contain one component")
    c = components[0]
    require(c["name"] == "cockpit" and c["version"] == version and c["commit"] == commit and c["repository"] == REPO, "Candidate source does not match requested release")
    digests = {a["name"]: a["digest"].removeprefix("sha256:") for a in c["artifacts"]}
    require(set(digests) == set(expected), "Native candidate is not the complete 41-asset set")
    require(all(re.fullmatch(r"[0-9a-f]{64}", d) for d in digests.values()), "Invalid retained artifact digest")
    return digests


def parse_sums(path):
    out = {}
    for line in path.read_text().splitlines():
        m = re.fullmatch(r"([0-9a-fA-F]{64})[ \t]+\*?([^\r\n]+)", line)
        require(m is not None, "Malformed checksum manifest")
        name = m[2].removeprefix("./")
        require("/" not in name and name not in out, "Ambiguous checksum target")
        out[name] = m[1].lower()
    return out


def verify_evidence(assets, expected, version):
    sums = parse_sums(assets / f"memql-{version}-SHA256SUMS")
    for target in PLATFORMS:
        for name in (f"memql-{version}-{target}.tar.gz",):
            require(sums.get(name) == sha(assets / name), "Headless archive checksum mismatch: " + name)
    for name in expected.values():
        if name.endswith(".sha256"):
            values = parse_sums(assets / name)
            require(len(values) == 1 and name[:-7] in values, "Detached checksum names wrong file")
            require(values[name[:-7]] == sha(assets / name[:-7]), "Detached checksum mismatch")
        if name.endswith(".cdx.json"):
            bom = json.loads((assets / name).read_text())
            require(bom.get("bomFormat") == "CycloneDX" and bom.get("specVersion"), "Invalid CycloneDX JSON")
            require(bom.get("metadata", {}).get("component", {}).get("version") == version, "SBOM version mismatch")
        if name.endswith(".cdx.xml"):
            bom = ET.fromstring((assets / name).read_bytes())
            require(bom.tag.endswith("}bom") and "cyclonedx.org/schema/bom/" in bom.tag, "Invalid CycloneDX XML")
            require(bom.findtext("{*}metadata/{*}component/{*}version") == version, "XML SBOM version mismatch")
    # Read archive members without extracting any paths or following links.
    for target in PLATFORMS:
        with tarfile.open(assets / f"memql-{version}-{target}.tar.gz") as tar:
            entries = [x for x in tar.getmembers() if x.name in ("memql", "./memql")]
            require(len(entries) == 1 and entries[0].isfile(), "Archive must contain one regular memql binary")
            require(hashlib.sha256(tar.extractfile(entries[0]).read()).hexdigest() == sha(assets / f"memql-{target}"), "Archive differs from matching raw binary")

    for target in ("darwin-arm64", "darwin-amd64"):
        for kind, bundle in (("app", "MemQL.app"), ("menubar", "MemQL Cockpit.app")):
            with tarfile.open(assets / f"memql-{kind}-{target}.tar.gz") as tar:
                name = bundle + "/Contents/Info.plist"
                entries = [x for x in tar.getmembers() if x.name in (name, "./" + name)]
                require(len(entries) == 1 and entries[0].isfile() and entries[0].size < 65536, "Missing/ambiguous bundle metadata")
                info = plistlib.loads(tar.extractfile(entries[0]).read())
                require(info.get("CFBundleShortVersionString") == version and info.get("CFBundleVersion") == version, "App/menu bundle version mismatch")
                expected_id = "com.znasllc.memql-worker" if kind == "app" else "com.znasllc.memql-cockpit-menubar"
                expected_executable = "MemQL" if kind == "app" else "MemQLCockpit"
                require(info.get("CFBundleIdentifier") == expected_id and info.get("CFBundleExecutable") == expected_executable, "App/menu bundle identity mismatch")
                if kind == "app":
                    helper_name = bundle + "/Contents/Library/LoginItems/MemQL Menu.app/Contents/Info.plist"
                    helpers = [x for x in tar.getmembers() if x.name in (helper_name, "./" + helper_name)]
                    require(len(helpers) == 1 and helpers[0].isfile() and helpers[0].size < 65536, "App login helper metadata missing")
                    helper = plistlib.loads(tar.extractfile(helpers[0]).read())
                    require(helper.get("CFBundleIdentifier") == "com.znasllc.memql-cockpit-menubar" and helper.get("CFBundleShortVersionString") == version, "App login helper identity/version mismatch")


def clean_env(home, bindir, guards):
    return {"HOME": str(home), "BIN_DIR": str(bindir), "PATH": str(guards) + ":/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin", "TMPDIR": str(home / "tmp"), "LANG": "C", "MEMQL_COCKPIT_CRED_STORE": "file"}


def command(argv, env=None, input_text=None, cwd=None, timeout=120):
    r = subprocess.run(argv, input=input_text, text=True, env=env, cwd=cwd, capture_output=True, timeout=timeout)
    require(r.returncode == 0, "Command failed: " + " ".join(map(str, argv[:3])) + "\n" + r.stderr[-3000:])
    return r.stdout


def mixed_catalog_fixture(selector, root):
    fixture = root / "selector-fixture"
    fixture.mkdir()
    curl = fixture / "curl"
    curl.write_text("#!" + sys.executable + "\n" + '''import json,sys
from pathlib import Path
args=sys.argv[1:];url=next(a for a in args if a.startswith("https://"))
base="https://api.github.com/repos/znasllc-io/memql/releases?per_page=100"
def rel(tag,**kw):return dict(tag_name=tag,draft=False,prerelease=False,**kw)
if url==base:
 body=[rel("v99.0.1"),rel("cockpit/v0.20.1")];header='Link: <'+base+'&page=2>; rel="next"\\r\\n'
elif url==base+"&page=2":
 body=[rel("cockpit/v0.21.0"),rel("cockpit/v0.19.9"),dict(tag_name="cockpit/v99.0.0",draft=True,prerelease=False),rel("cockpit/v1.0.0-rc.1")];header=""
else:sys.exit(22)
if "--dump-header" in args:Path(args[args.index("--dump-header")+1]).write_text(header)
if "-o" in args:Path(args[args.index("-o")+1]).write_text(json.dumps(body))
else:print(json.dumps(body))
''')
    curl.chmod(0o755)
    env = {"PATH": str(fixture) + ":/usr/bin:/bin", "HOME": str(fixture), "LANG": "C"}
    result = command([sys.executable, str(selector), "--api-url", API + "/releases?per_page=100"], env=env)
    require(result.strip() == "cockpit/v0.21.0", "Mixed catalog stable selection failed")
    return "passed: pagination, mixed engine/Cockpit order, draft/prerelease rejection"




def verify_go_metadata(build, stem, target, version, commit):
    require("github.com/znasllc-io/memql/cockpit/cmd/memql" in build, "Published executable is not the Cockpit CLI")
    target_os, target_arch = target.split("-")
    require(re.search(r"(?m)^\s*build\s+GOOS=" + target_os + "$", build) and re.search(r"(?m)^\s*build\s+GOARCH=" + target_arch + "$", build), "Go target platform/architecture mismatch")
    require(("-tags=computeruse" in build) == (stem == "memql-computeruse"), "Go build variant mismatch")
    require("main.revision=" + commit[:12] in build and "main.version=" + version in build, "Go build source/version stamp mismatch")
    if "vcs.revision=" in build:
        require("vcs.revision=" + commit in build, "Go VCS source mismatch")
    require("vcs.modified=true" not in build and "main.dirty=true" not in build, "Dirty release binary")


def signature_inspection(root, assets, version, commit, go):
    if platform.system() != "Darwin" or not Path("/usr/bin/codesign").exists():
        return {"inspected": False, "reason": "macOS codesign unavailable", "notarization": "not established"}
    require(hasattr(tarfile, "data_filter"), "Safe archive data filter required for static Mac signature inspection")
    inspection = root / "static-macos"
    inspection.mkdir()
    entries = []
    worker_proofs = []
    for target in ("darwin-arm64", "darwin-amd64"):
        paths = [(assets / (stem + "-" + target), stem + "-" + target) for stem in ("memql", "memql-computeruse")]
        for kind, bundle in (("app", "MemQL.app"), ("menubar", "MemQL Cockpit.app")):
            destination = inspection / (kind + "-" + target)
            destination.mkdir()
            with tarfile.open(assets / f"memql-{kind}-{target}.tar.gz") as archive:
                # Data filter refuses absolute paths, escaping symlinks and devices.
                # Extraction is static; no bundled script/executable is invoked.
                archive.extractall(destination, filter="data")
            paths.append((destination / bundle, kind + "-" + target))
            if kind == "app":
                worker = destination / bundle / "Contents/MacOS/MemQL"
                raw = assets / ("memql-computeruse-" + target)
                build = command([go, "version", "-m", str(worker)], timeout=30)
                verify_go_metadata(build, "memql-computeruse", target, version, commit)
                worker_proofs.append({"platform": target, "rawSHA256": sha(raw), "appWorkerSHA256": sha(worker),
                    "byteIdentical": sha(raw) == sha(worker), "buildMetadata": build,
                    "note": "App packaging signs a copy of the raw worker, so signature bytes may differ; retained artifact digests, exact Go metadata and deep strict signature verification are checked independently."})
                paths.append((destination / bundle / "Contents/Library/LoginItems/MemQL Menu.app", "app-menu-helper-" + target))
        for path, name in paths:
            detail = subprocess.run(["/usr/bin/codesign", "--display", "--verbose=4", str(path)], text=True, capture_output=True, timeout=30)
            verification = subprocess.run(["/usr/bin/codesign", "--verify", "--deep", "--strict", str(path)], text=True, capture_output=True, timeout=30)
            requirement = subprocess.run(["/usr/bin/codesign", "--display", "-r-", str(path)], text=True, capture_output=True, timeout=30)
            entitlement = subprocess.run(["/usr/bin/codesign", "--display", "--entitlements", ":-", str(path)], text=True, capture_output=True, timeout=30)
            description = detail.stdout + detail.stderr
            signed = detail.returncode == 0
            signing = "ad-hoc" if "Signature=adhoc" in description else "Developer ID" if "Authority=Developer ID" in description else "other signature" if signed else "unsigned"
            entry = {"asset": name, "signature": signing, "signatureInspectionSucceeded": signed,
                     "verificationPassed": verification.returncode == 0,
                     "details": description, "verification": verification.stdout + verification.stderr,
                     "entitlements": entitlement.stdout + entitlement.stderr,
                     "requirements": requirement.stdout + requirement.stderr,
                     "notarization": "not established; signature verification is not notarization"}
            entries.append(entry)
            if name.startswith(("app-", "menubar-")):
                require(signed and verification.returncode == 0, "Packaged Mac signature verification failed: " + name)
    return {"inspected": True, "entries": entries, "workerIdentity": worker_proofs, "appsLaunched": False, "permissionsChanged": False,
            "notarization": "not established; no Gatekeeper approval or first-launch qualification performed"}


def host_install(root, public, assets, version, commit):
    require(platform.system() == "Darwin", "Standalone execution is restricted to native macOS; Linux menu side effects require separate isolation")
    arch = {"arm64": "arm64", "x86_64": "amd64"}.get(platform.machine())
    require(arch is not None, "Unsupported native Mac architecture")
    home, bindir, guards = root / "isolated-home", root / "isolated-bin", root / "command-guards"
    for p in (home, bindir, guards):
        p.mkdir()  # Existing homes/binaries are refused, never reused.
    (home / "tmp").mkdir()
    denied = root / "forbidden-commands.jsonl"
    for name in ("launchctl", "systemctl", "sudo", "open", "osascript", "security", "kill", "pkill", "killall", "tccutil"):
        p = guards / name
        p.write_text("#!" + sys.executable + "\nimport json,sys\nfrom pathlib import Path\np=Path(" + repr(str(denied)) + ")\nwith p.open('a') as f:f.write(json.dumps({'command':Path(sys.argv[0]).name,'args':sys.argv[1:]})+'\\n')\nsys.exit(125)\n")
        p.chmod(0o755)
    env = clean_env(home, bindir, guards)
    env["MEMQL_COCKPIT_VERSION"] = version
    # Execute the unmodified public installer; pinned stable release avoids a
    # discovery race. Default latest was independently exercised beforehand.
    output = command(["/bin/sh"], env=env, input_text=(public / "cockpit/install.sh").read_text(), cwd=root, timeout=1900)
    require(not denied.exists(), "Installer attempted a forbidden host/service action")
    installed = bindir / "memql"
    require(sha(installed) == sha(assets / f"memql-darwin-{arch}"), "Installed binary differs from retained/public release")
    # This is the proven Cockpit package, never an engine/server executable.
    version_output = command([str(installed), "--version"], env=env, cwd=root, timeout=15).strip()
    require(re.fullmatch(r"memql " + re.escape(version) + r"\+g" + commit[:12] + r" \(headless\)", version_output), "Installed version/source/variant mismatch")
    require(not (home / ".memql").exists() and not (home / "Library").exists(), "Unexpected credential/policy/service state created")
    require(not denied.exists(), "Version command attempted a forbidden action")
    return {"version": version_output, "sha256": sha(installed), "home": str(home), "binDir": str(bindir), "serviceCommands": 0, "enrollment": "not performed"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("plan", "verify", "install"), default="plan")
    parser.add_argument("--execute", action="store_true")
    parser.add_argument("--tag", required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--candidate", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    require(re.fullmatch(r"cockpit/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", args.tag), "Exact stable Cockpit tag required")
    require(re.fullmatch(r"[0-9a-f]{40}", args.source_commit), "Exact reviewed source commit required")
    version = args.tag.removeprefix("cockpit/v")
    expected = expected_assets(version)
    if args.mode == "plan" or not args.execute:
        print(json.dumps({"ok": True, "planned": True, "tag": args.tag, "sourceCommit": args.source_commit, "assetCount": len(expected), "publicFiles": list(PUBLIC_FILES), "willInstall": args.mode == "install", "limitations": ["Never executes cross-OS/architecture binaries", "No computer-use permissions/app/service/enrollment qualification", "Fleet command fixtures are separate, synthetic-token-only tests"]}, indent=2))
        return
    require(args.candidate and args.candidate.is_file(), "Saved native candidate evidence required")
    digests = candidate_digests(args.candidate, version, args.source_commit, expected)
    root = args.output.resolve()
    require(not root.exists(), "Output must be a new dedicated directory")
    root.mkdir(parents=True)
    report = {"tag": args.tag, "sourceCommit": args.source_commit, "public": True, "assets": [], "installed": False}
    try:
        tag_ref = root / "tag-ref.json"
        fetch(API + "/git/ref/tags/" + urllib.parse.quote(args.tag, safe=""), tag_ref, 65536)
        reference = json.loads(tag_ref.read_text())
        require(reference.get("ref") == "refs/tags/" + args.tag and reference.get("object", {}).get("type") == "tag", "An annotated canonical component tag is required")
        tag_object = root / "tag-object.json"
        fetch(API + "/git/tags/" + reference["object"]["sha"], tag_object, 65536)
        annotated = json.loads(tag_object.read_text())
        require(annotated.get("tag") == args.tag and annotated.get("object", {}).get("type") == "commit" and annotated["object"]["sha"] == args.source_commit, "Annotated public tag does not resolve directly to reviewed release source")
        report["publicTag"] = {"object": reference["object"]["sha"], "commit": annotated["object"]["sha"], "annotated": True}
        metadata = root / "release.json"
        fetch(API + "/releases/tags/" + urllib.parse.quote(args.tag, safe=""), metadata, 2 * 1024 * 1024)
        release = json.loads(metadata.read_text())
        require(release.get("tag_name") == args.tag and release.get("draft") is False and release.get("prerelease") is False, "Expected stable release is not public")
        by_name = {a["name"]: a for a in release.get("assets", [])}
        require(len(by_name) == 41 and set(by_name) == set(expected.values()), "Public assets do not exactly match complete native set")
        public = root / "public-source"
        for name in PUBLIC_FILES:
            fetch(RAW + "/main/" + name, public / name, 512 * 1024)
            pinned = root / "pinned-source" / name
            fetch(RAW + "/" + args.source_commit + "/" + name, pinned, 512 * 1024)
            require(sha(public / name) == sha(pinned), "Public main installer changed since reviewed release source: " + name)
        selector = public / "cockpit/scripts/install/release-version.py"
        report["mixedCatalogFixture"] = mixed_catalog_fixture(selector, root)
        actual = command([sys.executable, str(selector), "--api-url", API + "/releases?per_page=100"], env={"PATH": "/usr/bin:/bin:/opt/homebrew/bin", "HOME": str(root), "LANG": "C"}).strip()
        require(actual == args.tag, "Default public stable selector does not choose this release")
        report["defaultSelection"] = actual
        assets = root / "assets"
        for artifact, name in expected.items():
            a = by_name[name]
            want_url = f"https://github.com/{REPO}/releases/download/{args.tag}/{name}"
            require(urllib.parse.unquote(a["browser_download_url"]) == want_url, "Asset URL escaped expected release")
            fetch(a["browser_download_url"], assets / name)
            digest = sha(assets / name)
            require(digest == digests[artifact], "Public asset differs from retained native artifact: " + name)
            require(a.get("digest") == "sha256:" + digest and a.get("size") == (assets / name).stat().st_size, "GitHub asset digest/size mismatch: " + name)
            report["assets"].append({"name": name, "sha256": digest, "size": a["size"]})
        verify_evidence(assets, expected, version)
        report["checksumsAndSBOMs"] = "all verified"
        go = shutil.which("go")
        require(go is not None, "Go required for static binary metadata inspection")
        report["buildMetadata"] = {}
        for target in PLATFORMS:
            for stem in ("memql", "memql-computeruse"):
                name = f"{stem}-{target}"
                build = command([go, "version", "-m", str(assets / name)], timeout=30)
                verify_go_metadata(build, stem, target, version, args.source_commit)
                report["buildMetadata"][name] = build
        report["macSignatures"] = signature_inspection(root, assets, version, args.source_commit, go)
        if args.mode == "install":
            report["installed"] = host_install(root, public, assets, version, args.source_commit)
        report["notExecuted"] = ["Linux and other architecture binaries", "computer-use binary/app", "worker enrollment or connection", "launchctl/systemctl", "existing credentials or policy", "Fleet live command execution"]
        report["ok"] = True
    except Exception as exc:
        report["ok"] = False
        report["error"] = str(exc)
        raise
    finally:
        (root / "qualification.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"ok": True, "report": str(root / "qualification.json"), "assetsVerified": 41, "installed": bool(report["installed"])}, indent=2))


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(json.dumps({"ok": False, "error": str(exc)}))
        sys.exit(5)
