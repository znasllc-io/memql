#!/usr/bin/env python3
"""Create nested Go modules for the memql module split (memql#3228).

Given a list of directories, for each one:
  - write go.mod (module path + go/toolchain lines)
  - add it to go.work
  - SEED it with the root module's external pins (see below)
  - resolve which OTHER modules it imports, and add require+replace for each
  - `GOWORK=off go mod tidy` in dependency order
  - add require+replace to the root go.mod
  - add a COPY line to both Dockerfiles
  - add an entry to KNOWN_GO_MOD_DIRS in db-gated-packages.sh

Idempotent: re-running with more directories extends the set.

WHY THE SEED STEP EXISTS (memql#3242)
-------------------------------------
`go mod tidy` in a module with NO requirements resolves every external import
to the LATEST release. Those versions then flow back into the root through the
`require`+`replace` edge, and MVS takes the max -- so carving out a directory
silently UPGRADED the shipped binaries' dependencies. Measured on the engine
tier before this step existed: `google.golang.org/grpc v1.81.0-dev -> v1.83.0`,
`go.opentelemetry.io/otel v1.41.0 -> v1.44.0`, `go-jose/v3` dropped from the
closure, +20 packages, every node binary +0.65%.

That is a dependency bump wearing a refactor's clothes: it lands in a PR whose
diff is `go.mod` files and file moves, reviewed as structural, and nobody looks
at a version. Dependency bumps are dependabot's job and are reviewed as such.

So each new module is seeded with the ROOT's requirement versions before it is
tidied. `go mod tidy` does not upgrade a requirement that already satisfies the
imports, so the seed wins and drops out again for anything the module does not
use. The result is version-neutral by construction rather than by inspection.
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


def root_external_pins():
    """The root module's external requirements, as {path: version}.

    Read from the root go.mod as it stands BEFORE this run touches it, so the
    seed reflects what the tree actually ships today. Internal modules are
    excluded -- those get their own require+replace from add_reqs.
    """
    import json
    out = sh(["go", "mod", "edit", "-json", os.path.join(REPO, "go.mod")])
    pins = {}
    for r in json.loads(out.stdout).get("Require") or []:
        path = r["Path"]
        if path == MOD or path.startswith(MOD + "/"):
            continue
        pins[path] = r["Version"]
    return pins


def seed_pins(d, pins):
    """Pin a new module to the root's external versions before tidying.

    Without this, `go mod tidy` resolves each import to the LATEST release and
    the upgrade propagates into the root through MVS -- see the module header.
    Seeded, tidy keeps these versions (they satisfy the imports) and drops the
    ones the module does not import.
    """
    modfile = os.path.join(REPO, d, "go.mod")
    args = ["go", "mod", "edit"]
    args += [f"-require={p}@{v}" for p, v in sorted(pins.items())]
    sh(args + [modfile])


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

    # Read BEFORE any go.mod is written, so the seed is the tree's current
    # pins rather than something this run produced.
    pins = root_external_pins()

    modules = existing_modules()
    for d in new_dirs:
        if d not in modules:
            write_go_mod(d, header)
            modules.append(d)
    modules = sorted(set(modules))
    update_go_work(modules)

    dep_map = {d: internal_deps(d, modules) for d in new_dirs}
    for d in new_dirs:
        seed_pins(d, pins)
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
