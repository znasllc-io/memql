#!/usr/bin/env python3
"""Create nested Go modules for the memql module split (memql#3228).

Given a list of directories, for each one:
  - write go.mod (module path + go/toolchain lines)
  - add it to go.work
  - resolve which OTHER modules it imports, and add require+replace for each
  - `GOWORK=off go mod tidy` in dependency order
  - add require+replace to the root go.mod
  - add a COPY line to both Dockerfiles
  - add an entry to KNOWN_GO_MOD_DIRS in db-gated-packages.sh

Idempotent: re-running with more directories extends the set.
"""
import os
import re
import subprocess
import sys

REPO = os.environ.get("MEMQL_REPO") or subprocess.run(
    ["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True, check=True
).stdout.strip()
MOD = "github.com/znasllc-io/memql"
GO_LINE = "go 1.26.1\n\ntoolchain go1.26.5\n"


def sh(args, cwd=REPO, env=None, check=True):
    e = dict(os.environ)
    if env:
        e.update(env)
    p = subprocess.run(args, cwd=cwd, env=e, capture_output=True, text=True)
    if check and p.returncode != 0:
        raise RuntimeError(f"{' '.join(args)} (cwd={cwd}) failed:\n{p.stdout}\n{p.stderr}")
    return p


def existing_modules():
    """Every module directory currently on disk, repo-relative, '.' for root."""
    out = []
    for dirpath, dirnames, filenames in os.walk(REPO):
        if ".git" in dirpath or "node_modules" in dirpath:
            continue
        if "go.mod" in filenames:
            rel = os.path.relpath(dirpath, REPO)
            out.append(rel)
    return sorted(out)


def owning_module(pkg_rel, modules):
    """Longest module dir that is a prefix of this repo-relative package path."""
    best = "."
    for m in modules:
        if m == ".":
            continue
        if pkg_rel == m or pkg_rel.startswith(m + "/"):
            if len(m) > len(best.replace(".", "")):
                best = m
    return best


def internal_deps(d, modules):
    """Module dirs (other than d) that packages under d import."""
    p = sh(["go", "list", "-deps", "./..."], cwd=os.path.join(REPO, d), check=False)
    if p.returncode != 0:
        p = sh(["go", "list", "-deps", f"{MOD}/{d}/..."], check=False)
        if p.returncode != 0:
            raise RuntimeError(f"cannot list deps for {d}:\n{p.stderr}")
    deps = set()
    for line in p.stdout.split():
        if not line.startswith(MOD + "/"):
            continue
        rel = line[len(MOD) + 1:]
        own = owning_module(rel, modules)
        if own != d and own != ".":
            deps.add(own)
    return sorted(deps)


def write_go_mod(d, header):
    path = os.path.join(REPO, d, "go.mod")
    with open(path, "w") as f:
        f.write(header)
        f.write(f"module {MOD}/{d}\n\n")
        f.write(GO_LINE)


def add_reqs(d, deps):
    modfile = os.path.join(REPO, d, "go.mod")
    for dep in deps:
        rel = os.path.relpath(os.path.join(REPO, dep), os.path.join(REPO, d))
        # `go mod edit` rejects a bare relative path: a local replacement must
        # be spelled ./x or ../x, and relpath yields "gen" for a child.
        if not rel.startswith("."):
            rel = "./" + rel
        sh(["go", "mod", "edit",
            f"-require={MOD}/{dep}@v0.0.0",
            f"-replace={MOD}/{dep}={rel}", modfile])


def topo(dirs, dep_map):
    """Order so a module's in-set dependencies are tidied before it."""
    done, out = set(), []
    remaining = list(dirs)
    while remaining:
        progressed = False
        for d in list(remaining):
            if all(x in done or x not in dirs for x in dep_map[d]):
                out.append(d)
                done.add(d)
                remaining.remove(d)
                progressed = True
        if not progressed:
            raise RuntimeError(f"dependency cycle among: {remaining}")
    return out


def update_go_work(modules):
    lines = open(os.path.join(REPO, "go.work")).read().split("\n")
    head, i = [], 0
    while i < len(lines):
        head.append(lines[i])
        if lines[i].strip() == "use (":
            break
        i += 1
    tail = []
    while i < len(lines):
        if lines[i].strip() == ")":
            tail = lines[i:]
            break
        i += 1
    body = ["\t."] + [f"\t./{m}" for m in sorted(m for m in modules if m != ".")]
    open(os.path.join(REPO, "go.work"), "w").write("\n".join(head + body + tail))


def update_dockerfiles(modules):
    nested = sorted(m for m in modules if m != ".")
    marker = "# BuildKit cache mounts"
    for df, anchor in (("Dockerfile", marker), ("cmd/deploy-gate-check/Dockerfile", "RUN go mod download")):
        path = os.path.join(REPO, df)
        text = open(path).read()
        block = "".join(f"COPY {m}/go.* ./{m}/\n" for m in nested)
        # Replace the existing contiguous COPY <mod>/go.mod block.
        text = re.sub(r"(?m)^COPY [^\n]*/go\.\* \./[^\n]*/\n(?:COPY [^\n]*/go\.\* \./[^\n]*/\n)*",
                      block, text, count=1)
        open(path, "w").write(text)


def update_known_dirs(modules):
    path = os.path.join(REPO, "scripts/ci/db-gated-packages.sh")
    text = open(path).read()
    entries = "\n".join(f'\t"{m}"' for m in sorted(m for m in modules if m != "."))
    new = f'readonly KNOWN_GO_MOD_DIRS=(\n\t"."\n{entries}\n)'
    text = re.sub(r"readonly KNOWN_GO_MOD_DIRS=\(\n(?:[^\n]*\n)*?\)", new, text, count=1)
    open(path, "w").write(text)


def main():
    new_dirs = sys.argv[1:]
    if not new_dirs:
        print("usage: mkmods.py <dir> [<dir>...]", file=sys.stderr)
        return 2

    header = ("// Part of the memql module split (memql#3228). Tier assignment and\n"
              "// rationale: docs/ci-design.md, section D3.\n")

    modules = existing_modules()
    for d in new_dirs:
        if d not in modules:
            write_go_mod(d, header)
            modules.append(d)
    modules = sorted(set(modules))
    update_go_work(modules)

    dep_map = {d: internal_deps(d, modules) for d in new_dirs}
    for d in new_dirs:
        add_reqs(d, dep_map[d])

    for d in topo(new_dirs, dep_map):
        print(f"  tidy {d}  <- {' '.join(dep_map[d]) or '(L0)'}")
        sh(["go", "mod", "tidy"], cwd=os.path.join(REPO, d), env={"GOWORK": "off"})

    root = os.path.join(REPO, "go.mod")
    for d in new_dirs:
        sh(["go", "mod", "edit", f"-require={MOD}/{d}@v0.0.0", f"-replace={MOD}/{d}=./{d}", root])
    sh(["go", "mod", "tidy"], env={"GOWORK": "off"})

    # Align every module on the workspace's selected versions.
    #
    # Each `go mod tidy` above ran with GOWORK=off, so each module resolved its
    # external dependencies by its OWN minimal-version selection -- a module
    # needing only grpc's basics picks an older grpc than the root does. The
    # drift is invisible locally, because the workspace resolves the maximum
    # across modules and every build stays green over go.mod files that
    # disagree. What notices is Dependabot, which reads each go.mod separately
    # and opens one PR per (module, dependency) to close each gap.
    sh(["go", "work", "sync"])

    update_dockerfiles(modules)
    update_known_dirs(modules)
    print(f"total modules: {len(modules)}")


if __name__ == "__main__":
    sys.exit(main() or 0)
