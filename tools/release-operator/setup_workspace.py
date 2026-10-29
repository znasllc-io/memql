#!/usr/bin/env python3
"""Bind this helper to an explicit local canonical checkout; never build or connect."""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys


class ParameterError(ValueError):
    pass


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise ParameterError(message)


def read_workspace(source, helper):
    if not shutil.which("go"):
        raise FileNotFoundError("Go is required")
    work = source / "go.work"
    if not work.is_file():
        raise FileNotFoundError("Canonical checkout go.work is missing")
    env = dict(os.environ, GOTOOLCHAIN="local", GOWORK="off")
    result = subprocess.run(
        ["go", "work", "edit", "-json", str(work)], cwd=helper, env=env,
        capture_output=True, text=True, timeout=30, check=False,
    )
    if result.returncode:
        raise ValueError("Local Go could not parse canonical go.work; no toolchain download attempted")
    data = json.loads(result.stdout)
    if data.get("Replace"):
        raise ValueError("Canonical workspace replacements require review before portable setup")
    version = data.get("Go", "")
    toolchain = data.get("Toolchain", "")
    if not re.fullmatch(r"1\.[0-9]+(?:\.[0-9]+)?", version):
        raise ValueError("Unexpected canonical Go version")
    if toolchain and not re.fullmatch(r"go1\.[0-9]+(?:\.[0-9]+)?", toolchain):
        raise ValueError("Unexpected canonical toolchain")
    modules = []
    for entry in data.get("Use", []):
        module = (source / entry["DiskPath"]).resolve()
        if not module.is_relative_to(source) or not (module / "go.mod").is_file():
            raise ValueError("Workspace module is missing or outside canonical checkout")
        if module not in modules:
            modules.append(module)
    if source not in modules or source / "cockpit" not in modules:
        raise ValueError("Canonical root and Cockpit modules are required")
    expected = "module github.com/znasllc-io/memql/cockpit/releaseoperator"
    if expected not in (helper / "go.mod").read_text().splitlines():
        raise ValueError("Helper module must retain Cockpit internal-import scope")
    if helper in modules:
        raise ValueError("Helper is already part of canonical workspace")
    modules.append(helper)
    lines = ["// Machine-local binding; do not commit.", "go " + version]
    if toolchain:
        lines.append("toolchain " + toolchain)
    lines.extend(["", "use ("] + ["\t" + json.dumps(str(p), ensure_ascii=False) for p in modules] + [")", ""])
    return "\n".join(lines), len(modules), version, toolchain


def write_workspace(path, text):
    if path.is_symlink():
        raise ValueError("Refused symbolic-link workspace destination")
    if path.exists():
        if path.read_text() == text:
            return False
        raise ValueError("Existing workspace differs; review and remove it before rebinding")
    with path.open("x", encoding="utf-8") as handle:
        os.chmod(path, 0o600)
        handle.write(text)
    return True


def main():
    try:
        parser = Parser(description=__doc__)
        parser.add_argument("--source-tree", required=True, type=Path)
        parser.add_argument("--execute", action="store_true", help="Create only the local go.work binding")
        args = parser.parse_args()
        source = args.source_tree.expanduser().resolve(strict=True)
        helper = Path(__file__).resolve().parent
        text, count, version, toolchain = read_workspace(source, helper)
        destination = helper / "go.work"
        created = write_workspace(destination, text) if args.execute else False
        print(json.dumps({"ok": True, "planned": not args.execute, "created": created,
                          "workspace": str(destination), "moduleCount": count,
                          "goVersion": version, "toolchain": toolchain,
                          "buildPerformed": False, "authenticatedCallPerformed": False}))
        return 0
    except ParameterError as error:
        print(json.dumps({"ok": False, "category": "bad_parameter", "error": str(error)}))
        return 2
    except FileNotFoundError as error:
        print(json.dumps({"ok": False, "category": "prerequisite", "error": str(error)}))
        return 4
    except (ValueError, KeyError, TypeError) as error:
        print(json.dumps({"ok": False, "category": "refused", "error": str(error)}))
        return 3
    except (OSError, subprocess.SubprocessError):
        print(json.dumps({"ok": False, "category": "operation_failed", "error": "Local workspace setup failed"}))
        return 5


if __name__ == "__main__":
    sys.exit(main())
