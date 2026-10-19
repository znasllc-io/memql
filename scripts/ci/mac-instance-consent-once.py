#!/usr/bin/env python3
"""One-time owner-requested maintenance. This branch must never be merged."""
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import signal
import stat
import subprocess
import tempfile

REPOSITORY = "znasllc-io/memql-znas"


def updated_policy(body):
    # Preserve every unrelated byte and refuse unfamiliar YAML instead of
    # reserializing or broadening a machine policy.
    lines = body.splitlines(keepends=True)
    starts = [i for i, line in enumerate(lines) if re.fullmatch(r"pipelines:\s*(?:#.*)?\n?", line)]
    if len(starts) != 1:
        raise ValueError("expected one explicit pipelines block")
    start = starts[0] + 1
    end = next((i for i in range(start, len(lines)) if re.match(r"[^\s#]", lines[i])), len(lines))
    section = lines[start:end]
    if sum(bool(re.fullmatch(r"  allow: true\s*(?:#.*)?\n?", line)) for line in section) != 1:
        raise ValueError("existing explicit pipeline permission is required")
    indexes = [i for i in range(start, end) if lines[i].startswith("  repos:")]
    if len(indexes) != 1:
        raise ValueError("expected one repository allowlist")
    index = indexes[0]
    line = lines[index]
    inline = re.fullmatch(r"  repos: \[(.*)\](\s*(?:#.*)?\n?)", line)
    if inline:
        repositories = [x.strip().strip('\"\'') for x in inline[1].split(",")]
        replacement = lambda: lines.__setitem__(index, "  repos: [" + inline[1] + ", " + REPOSITORY + "]" + inline[2])
    elif re.fullmatch(r"  repos:\s*(?:#.*)?\n?", line):
        cursor = index + 1
        repositories = []
        while cursor < end:
            candidate = lines[cursor]
            if not candidate.strip() or candidate.lstrip().startswith("#"):
                cursor += 1
                continue
            match = re.fullmatch(r"    - ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)\s*(?:#.*)?\n?", candidate)
            if not match:
                break
            repositories.append(match[1])
            cursor += 1
        replacement = lambda: lines.insert(index + 1, "    - " + REPOSITORY + "\n")
    else:
        raise ValueError("unsupported repository allowlist layout")
    if not repositories or any(not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", r) for r in repositories):
        raise ValueError("repository allowlist must be explicit and nonempty")
    lowered = [r.lower().removesuffix(".git") for r in repositories]
    if "znasllc-io/memql" not in lowered:
        raise ValueError("the existing MemQL repository permission is missing")
    if REPOSITORY not in lowered:
        replacement()
    return "".join(lines)


def main():
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        raise RuntimeError("this maintenance is only for the enrolled Apple Silicon Mac")
    if hashlib.sha256(platform.node().lower().encode()).hexdigest() != os.environ["MEMQL_MAINTENANCE_HOST_SHA256"]:
        raise RuntimeError("this is not the owner-selected machine")
    home = Path.home()
    policy = home / ".memql/policy.yaml"
    metadata = policy.lstat()
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_uid != os.getuid():
        raise RuntimeError("the policy must be a regular file owned by the current user")
    service = subprocess.check_output(["launchctl", "print", f"gui/{os.getuid()}/com.znasllc.memql-worker"], text=True)
    pids = re.findall(r"^\s*pid = ([0-9]+)\s*$", service, re.M)
    if len(pids) != 1:
        raise RuntimeError("the enrolled Cockpit service did not report one PID")
    pid = int(pids[0])
    command = subprocess.check_output(["ps", "-p", str(pid), "-o", "command="], text=True)
    uid = subprocess.check_output(["ps", "-p", str(pid), "-o", "uid="], text=True).strip()
    if "worker run" not in command or uid != str(os.getuid()):
        raise RuntimeError("the service PID is not this user's running Cockpit")
    original = policy.read_bytes()
    revised = updated_policy(original.decode()).encode()
    changed = revised != original
    temporary = None
    try:
        if changed:
            fd, temporary = tempfile.mkstemp(prefix=".cockpit-policy-", dir=policy.parent)
            with os.fdopen(fd, "wb") as stream:
                os.fchmod(stream.fileno(), stat.S_IMODE(metadata.st_mode))
                stream.write(revised)
                stream.flush()
                os.fsync(stream.fileno())
            if policy.lstat().st_ino != metadata.st_ino or policy.read_bytes() != original:
                raise RuntimeError("the policy changed concurrently; no update applied")
            os.replace(temporary, policy)
            temporary = None
        if policy.read_bytes() != revised:
            raise RuntimeError("the saved policy differs from the reviewed update")
        os.kill(pid, signal.SIGHUP)
        interpreters = []
        for executable in ["/bin/bash", "/opt/homebrew/bin/bash", "/usr/local/bin/bash"]:
            if Path(executable).is_file():
                major = subprocess.check_output([executable, "-c", 'printf "%s" "${BASH_VERSINFO[0]}"'], text=True).strip()
                if not major.isdigit():
                    raise RuntimeError("interpreter version could not be read")
                interpreters.append({"path": executable, "major": int(major)})
        receipt = {"interpreters": interpreters, "repository": REPOSITORY, "added": changed, "priorPermissionPreserved": True,
                   "beforeSHA256": hashlib.sha256(original).hexdigest(), "afterSHA256": hashlib.sha256(revised).hexdigest(),
                   "reloadRequested": True, "temporaryFilesRemoved": True}
        Path("maintenance-receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
        print(json.dumps(receipt))
    finally:
        if temporary is not None:
            Path(temporary).unlink(missing_ok=True)


if __name__ == "__main__":
    main()
