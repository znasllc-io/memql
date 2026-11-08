#!/usr/bin/env bash
set -euo pipefail

# Tokenless, one-time bootstrap for an existing Linux worker. The downloaded
# command owns installation, rollback and reconnection; this wrapper only
# discovers the existing installation and verifies the bootstrap executable.
readonly UPGRADE_REPOSITORY="znasllc-io/memql"

function show_help() {
    cat <<'EOF'
Usage: upgrade-linux.sh --version=X.Y.Z --confirm=restart-worker

Upgrade the existing MemQL user service without pairing again. Finish active
work first: older workers cannot report whether they are busy. The installed
build variant, service definition, enrollment and consent remain unchanged.

The version must name a published cockpit/vX.Y.Z release. This command supports
user-writable installations; it never runs the updater as root or changes a
protected installation into a user-writable one.
EOF
}

function bootstrap_fail() {
    local code="$1" message="$2"
    printf 'ERROR: %s\n' "$message" >&2
    # Only fixed messages reach this pre-runtime path.
    printf '{"ok":false,"capability":"cockpit.upgrade.linux","changed":false,"result":{},"error":{"code":%s,"message":"%s"}}\n' "$code" "$message"
    exit "$code"
}

function load_capability_runtime() {
    local entry="${BASH_SOURCE[0]:-}" runtime="" version="" arg temporary=""
    if [[ -f "$entry" && "${entry##*/}" == upgrade-linux.sh ]]; then
        runtime="$(cd "$(dirname "$entry")/../../.." && pwd)/scripts/lib/capability.sh"
    fi
    if [[ ! -f "$runtime" ]]; then
        for arg in "$@"; do
            case "$arg" in --version=*) version="${arg#*=}" ;; esac
        done
        [[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] \
            || bootstrap_fail 2 'Pass --version=X.Y.Z for the published Cockpit release.'
        command -v curl >/dev/null 2>&1 || bootstrap_fail 4 'curl is required.'
        temporary="$(mktemp)" || bootstrap_fail 4 'Could not create private download storage.'
        runtime="$temporary"
        if ! curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 60 --max-filesize 65536 \
            "https://raw.githubusercontent.com/${UPGRADE_REPOSITORY}/refs/tags/cockpit/v${version}/scripts/lib/capability.sh" -o "$runtime"; then
            rm -f "$temporary"
            bootstrap_fail 4 'Could not load the runtime from the selected release. Nothing was changed.'
        fi
    fi
    # shellcheck source=../../../scripts/lib/capability.sh
    source "$runtime"
    [[ -z "$temporary" ]] || rm -f "$temporary"
}

function perform_upgrade() {
    python3 - "$1" "$2" "$3" <<'PY'
import hashlib
import gzip
import json
import os
import re
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.parse
import urllib.request
from pathlib import Path

VERSION, CONFIRM, ARCH = sys.argv[1:]
MAX_BINARY = 512 * 1024 * 1024
RESULT = None


class Refused(Exception):
    def __init__(self, message, code=3, receipt=None):
        super().__init__(message)
        self.code = code
        self.receipt = dict(receipt if receipt is not None else (RESULT or {"updated": False}))
        self.receipt.update(ok=False, error=message)


def command(arguments, timeout=30):
    try:
        result = subprocess.run(arguments, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise Refused("Could not query the existing worker or menu service. Check its status and retry.", 4)
    if result.returncode != 0:
        raise Refused("The installed worker or its user service did not respond. Start the existing worker and retry.", 4)
    if len(result.stdout) > 1024 * 1024:
        raise Refused("The installed worker returned an invalid response.")
    return result.stdout.decode("utf-8", errors="strict").strip()


def installed_executable():
    # Read only ExecStart: querying the whole service could expose its token
    # environment. The friendly memql symlink remains exactly as installed.
    value = command(["systemctl", "--user", "show", "--property=ExecStart", "--value", "memql-worker.service"])
    if value.count("path=") != 1 or not re.match(r"^\s*\{\s*path=", value):
        raise Refused("The MemQL user service has an ambiguous executable. Repair its existing installation first.")
    match = re.match(r"^\s*\{\s*path=(.*?)\s*;", value)
    if match is None:
        raise Refused("The MemQL user service did not report its executable.")
    raw = match.group(1)
    # systemctl escapes whitespace and special bytes in the structured path.
    raw = re.sub(r"\\x([0-9a-fA-F]{2})", lambda m: chr(int(m.group(1), 16)), raw)
    if "\\" in raw or not os.path.isabs(raw) or any(ord(c) < 32 for c in raw):
        raise Refused("The MemQL service executable path is unsupported. Repair its existing installation first.")
    executable = Path(raw).resolve(strict=True)
    if not stat.S_ISREG(executable.stat().st_mode) or not os.access(executable, os.X_OK):
        raise Refused("The MemQL service does not point to an executable file.")
    if not os.access(executable.parent, os.W_OK | os.X_OK):
        raise Refused("This service uses a protected installation. Use its administrator-managed upgrade procedure, or explicitly migrate to a user-local installation before enabling menu updates. Do not run this command with sudo.")
    return executable


class TrustedRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        target = urllib.parse.urlparse(newurl)
        if target.scheme != "https" or target.username is not None or target.password is not None:
            raise Refused("The release download used an untrusted redirect.")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def download(address, destination, limit):
    request = urllib.request.Request(address, headers={"User-Agent": "MemQL-Cockpit-Upgrade"})
    digest = hashlib.sha256()
    count = 0
    deadline = time.monotonic() + 1800
    try:
        with urllib.request.build_opener(TrustedRedirects()).open(request, timeout=120) as response, destination.open("xb") as output:
            while True:
                block = response.read(1024 * 1024)
                if not block:
                    break
                if time.monotonic() > deadline:
                    raise Refused("The release download timed out. Nothing was installed.", 5)
                count += len(block)
                if count > limit:
                    raise Refused("The release download exceeded its size limit.", 5)
                digest.update(block)
                output.write(block)
    except Refused:
        raise
    except Exception:
        raise Refused("Could not download the selected Cockpit release. Nothing was installed; check the release and retry.", 5)
    return digest.hexdigest()


def invoke_updater(arguments):
    global RESULT
    # The updater owns download/restart deadlines and rollback. A wrapper
    # timeout must never kill it halfway through recovering the worker.
    process = subprocess.run(arguments, stdout=subprocess.PIPE, stderr=sys.stderr, check=False)
    try:
        receipt = json.loads(process.stdout)
        if not isinstance(receipt, dict):
            raise ValueError("not an object")
    except (ValueError, UnicodeDecodeError):
        raise Refused("The updater returned no readable result. Check the existing worker before retrying.", 5)
    RESULT = receipt
    if process.returncode or not receipt.get("ok"):
        raise Refused(receipt.get("error") or "The update did not finish. Check the existing worker before retrying.", 5, receipt)
    return receipt


def finish(executable, receipt):
    global RESULT
    RESULT = receipt
    # The worker and menu share the installed binary on Linux. Refresh an
    # existing active menu only; do not add a tray or enable a paused one.
    menu_state = subprocess.run(["systemctl", "--user", "show", "--property=ActiveState", "--value", "memql-menu.service"],
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=15, check=False)
    receipt["menu_restarted"] = False
    if menu_state.returncode == 0 and menu_state.stdout.strip() == b"active":
        menu_exec = command(["systemctl", "--user", "show", "--property=ExecStart", "--value", "memql-menu.service"])
        menu_match = re.match(r"^\s*\{\s*path=(.*?)\s*;", menu_exec)
        menu_path = re.sub(r"\\x([0-9a-fA-F]{2})", lambda m: chr(int(m.group(1), 16)), menu_match.group(1)) if menu_match else ""
        if menu_exec.count("path=") != 1 or not os.path.isabs(menu_path) or Path(menu_path).resolve() != executable:
            receipt["ok"] = False
            receipt["error"] = "Cockpit updated, but its active menu points to another installation. Open the menu belonging to this worker."
            raise Refused(receipt["error"], 5, receipt)
        restarted = subprocess.run(["systemctl", "--user", "restart", "memql-menu.service"],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30, check=False)
        if restarted.returncode != 0:
            receipt["ok"] = False
            receipt["error"] = "Cockpit updated, but its menu could not restart. Run: systemctl --user restart memql-menu.service"
            raise Refused(receipt["error"], 5, receipt)
        receipt["menu_restarted"] = True
    print(json.dumps(receipt))


def main():
    if os.getuid() == 0:
        raise Refused("Run this command as the signed-in desktop user, without sudo. The worker belongs to that user.")
    executable = installed_executable()
    output = command([str(executable), "--version"])
    match = re.fullmatch(r"memql ((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:\+[0-9A-Za-z.-]+)?) \((headless|computeruse)\)", output)
    if match is None:
        raise Refused("The installed worker did not report a recognized version and build variant.")
    current, variant = match.groups()
    if tuple(map(int, VERSION.split("."))) < tuple(map(int, current.split("+")[0].split("."))):
        raise Refused("The selected release is older than the installed worker. Downgrades are refused.")
    # Always delegate no-op and interrupted-update decisions to the verified
    # helper. Its private journal binds recovery to the saved home choices;
    # a version/path-only shortcut cannot establish a healthy current worker.
    base = "https://github.com/znasllc-io/memql/releases/download/cockpit/v" + VERSION
    # Offline fixtures can use only an explicit loopback server. Production
    # URLs and redirects stay HTTPS; no arbitrary download source is accepted.
    fixture = os.environ.get("MEMQL_UPGRADE_TEST_DOWNLOAD_BASE")
    if fixture:
        parsed = urllib.parse.urlparse(fixture)
        if os.environ.get("MEMQL_UPGRADE_ALLOW_LOOPBACK_HTTP") != "1" or parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "::1") or parsed.username is not None or parsed.password is not None or parsed.query or parsed.fragment:
            raise Refused("The test release source must be an explicitly enabled loopback server.")
        base = fixture.rstrip("/")
    if variant == "computeruse":
        asset = "memql-computeruse-linux-" + ARCH
        sums = asset + ".sha256"
    else:
        asset = "memql-" + VERSION + "-linux-" + ARCH + ".tar.gz"
        sums = "memql-" + VERSION + "-SHA256SUMS"
    print("INFO: Downloading the selected " + variant + " release; saved connections and permissions are kept.", file=sys.stderr)
    with tempfile.TemporaryDirectory(prefix="memql-upgrade-") as temporary:
        scratch = Path(temporary)
        manifest = scratch / "checksums"
        download(base + "/" + sums, manifest, 2 * 1024 * 1024)
        entries = [line.split() for line in manifest.read_text().splitlines()]
        checksums = [entry[0].lower() for entry in entries if len(entry) == 2 and entry[1].lstrip("*") == asset]
        if len(checksums) != 1 or re.fullmatch(r"[0-9a-f]{64}", checksums[0]) is None:
            raise Refused("The selected release has no unambiguous checksum for this build.", 5)
        payload = scratch / asset
        if download(base + "/" + asset, payload, MAX_BINARY) != checksums[0]:
            raise Refused("Cockpit checksum verification failed. Nothing was installed.", 5)
        bootstrap = payload
        if variant == "headless":
            class BoundedArchive:
                def __init__(self, source):
                    self.source = source
                    self.count = 0

                def read(self, size):
                    block = self.source.read(size)
                    self.count += len(block)
                    if self.count > MAX_BINARY:
                        raise Refused("The release archive exceeded its expanded size limit.", 5)
                    return block

            bootstrap = scratch / "memql"
            found = False
            with gzip.open(payload, "rb") as compressed, tarfile.open(fileobj=BoundedArchive(compressed), mode="r|") as archive:
                for count, member in enumerate(archive, 1):
                    if count > 10000:
                        raise Refused("The release archive contains too many entries.", 5)
                    if member.name not in ("memql", "./memql"):
                        continue
                    if found or not member.isfile() or not 0 < member.size <= MAX_BINARY:
                        raise Refused("The selected archive does not contain one regular memql executable.", 5)
                    found = True
                    with archive.extractfile(member) as source, bootstrap.open("xb") as destination:
                        remaining = member.size
                        while remaining:
                            block = source.read(min(1024 * 1024, remaining))
                            if not block:
                                raise Refused("The selected archive is incomplete.", 5)
                            destination.write(block)
                            remaining -= len(block)
            if not found:
                raise Refused("The selected archive is missing its memql executable.", 5)
        bootstrap.chmod(0o755)
        actual = command([str(bootstrap), "--version"])
        expected = re.fullmatch(r"memql " + re.escape(VERSION) + r"(?:\+[0-9A-Za-z.-]+)? \(" + variant + r"\)", actual)
        if expected is None:
            raise Refused("The downloaded worker does not match the selected version and installed variant.", 5)
        print("INFO: Finishing the one-time upgrade. The existing worker will restart.", file=sys.stderr)
        args = [str(bootstrap), "update", "--bootstrap", "--installed-executable=" + str(executable),
                "--installed-version=" + current, "--version=" + VERSION, "--confirm=" + CONFIRM, "--json"]
        receipt = invoke_updater(args)
        finish(executable, receipt)


try:
    main()
except Refused as failure:
    print(json.dumps(failure.receipt))
    sys.exit(failure.code)
except Exception:
    receipt = dict(RESULT or {"updated": False})
    receipt.update(ok=False, error="The worker update finished, but the menu could not be refreshed. Check its status and reopen MemQL Cockpit." if RESULT else "The upgrade could not continue. Check the existing worker and retry; enrollment and consent were not edited.")
    print(json.dumps(receipt))
    sys.exit(5)
PY
}

function main() {
    local arg version confirm architecture result code
    for arg in "$@"; do
        case "$arg" in --help|-h) show_help; return 0 ;; esac
    done
    load_capability_runtime "$@"
    cap_init "cockpit.upgrade.linux" "Upgrade an existing Linux Cockpit without pairing or changing consent."
    cap_spec_param_required "version" "Published Cockpit release X.Y.Z"
    cap_spec_param_required "confirm" "restart-worker: finish active work before upgrading an older worker"
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    version="$(cap_param version)"; confirm="$(cap_param confirm)"
    [[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || cap_fail 2 "version must be X.Y.Z"
    [[ "$confirm" == restart-worker ]] || cap_fail 3 "Finish active work, then pass --confirm=restart-worker for this one-time upgrade"
    [[ "$(uname -s)" == Linux ]] || cap_fail 3 "Use upgrade-mac.sh for a Mac"
    command -v python3 >/dev/null 2>&1 || cap_fail 4 "Python 3 is required"
    command -v systemctl >/dev/null 2>&1 || cap_fail 4 "The existing MemQL user service requires systemctl"
    case "$(uname -m)" in
        x86_64|amd64) architecture=amd64 ;;
        aarch64|arm64) architecture=arm64 ;;
        *) cap_fail 4 "This processor is not supported" ;;
    esac
    if result="$(perform_upgrade "$version" "$confirm" "$architecture")"; then
        if [[ "$(python3 -c 'import json,sys; r=json.loads(sys.argv[1]); print(str(r.get("updated", False) or r.get("menu_restarted", False)).lower())' "$result")" == true ]]; then cap_changed; fi
        cap_ok "$result"
    else
        code=$?
        cap_result_set_raw outcome "$result"
        if [[ "$(python3 -c 'import json,sys; print(str(json.loads(sys.argv[1]).get("updated", False)).lower())' "$result")" == true ]]; then cap_changed; fi
        cap_fail "$code" "$(python3 -c 'import json,sys; print(json.loads(sys.argv[1]).get("error", "Upgrade failed"))' "$result")"
    fi
}

main "$@"
