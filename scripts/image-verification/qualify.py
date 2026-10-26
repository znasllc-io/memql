#!/usr/bin/env python3
"""Reviewed two-phase owner setup; never installs tools, units, or an attestation.

prepare builds the pinned stdlib tools while the worker keeps running. The
separately reviewed root provisioning block runs between prepare and qualify.
qualify stops the exact worker only before the real fixture; uncertain cleanup leaves it stopped. A preparation failure preserves its fence without claiming a worker stop.
"""
import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import signal
import stat
import subprocess
import sys
import time
import types

CAPABILITY = "image-verification.qualify"
GO_VERSION = "go1.27.3"
MIN_FREE_MIB = 20480
RUN_FLOOR_MIB = 5120
LOG_LIMIT = 64 << 20
MAX_SETUP_AGE_SECONDS = 3600
FIXTURE = "TestInstalledImageVerificationQualification"
LOADED_PROPERTIES = ("ExecStart", "Environment", "EnvironmentFiles", "User", "Group", "UMask", "NoNewPrivileges", "ProtectSystem", "ProtectHome", "PrivateUsers", "RestrictNamespaces", "Delegate", "CPUAccounting", "MemoryAccounting", "TasksAccounting", "KillMode", "Restart", "TimeoutStopUSec", "WorkingDirectory", "RootDirectory")


class Refused(RuntimeError):
    pass


class Parser(argparse.ArgumentParser):
    def error(self, message):
        print("MemQL: " + message, file=sys.stderr)
        print(json.dumps({"ok":False,"capability":CAPABILITY,"changed":False,"result":{},"error":{"code":2,"message":message}}))
        raise SystemExit(2)


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def regular(path, maximum, owner=None):
    p = Path(path)
    for parent in [p, *p.parents]:
        if parent.is_symlink():
            raise Refused("A setup path crosses a symbolic link")
    s = p.stat()
    if not stat.S_ISREG(s.st_mode) or s.st_nlink != 1 or s.st_size > maximum:
        raise Refused("A setup file is not a bounded regular file")
    if owner is not None and s.st_uid != owner:
        raise Refused("A setup file belongs to another account")
    return p


def read_json(path, maximum=1 << 20, owner=None):
    value = json.loads(regular(path, maximum, owner).read_bytes())
    if not isinstance(value, dict):
        raise Refused("A setup record is not an object")
    return value


def save(path, value, *, replace=False):
    p = Path(path)
    target = p.with_name(p.name + "." + secrets.token_hex(8)) if replace else p
    fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as out:
        json.dump(value, out, sort_keys=True)
        out.write("\n")
        out.flush()
        os.fsync(out.fileno())
    if replace:
        os.replace(target, p)
    fd = os.open(p.parent, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def load_plan(path, source):
    p = read_json(path)
    required = {"schemaVersion", "sourceSHA", "sourceFiles", "systemctl", "worker", "fixtures", "idleReceipt", "go"}
    if set(p) != required or p["schemaVersion"] != 1 or not re.fullmatch(r"[0-9a-f]{40}", p["sourceSHA"] or ""):
        raise Refused("The exact reviewed source plan is not ready")
    files = p["sourceFiles"]
    guard = "scripts/lib/host_build_guard.py"
    main = "cockpit/cmd/memql-imageverify/main.go"
    if not isinstance(files, dict) or guard not in files or main not in files or not 3 <= len(files) <= 128:
        raise Refused("The reviewed source inventory is incomplete")
    for name, expected in files.items():
        allowed = name in (guard, main) or re.fullmatch(r"cockpit/internal/worker/imageverify/[A-Za-z0-9_]+\.go", name)
        if not allowed or not re.fullmatch(r"[0-9a-f]{64}", expected or ""):
            raise Refused("The reviewed source inventory is invalid")
        if digest(regular(source / name, 2 << 20)) != expected:
            raise Refused("Reviewed source bytes changed")
    actual = {str(f.relative_to(source)) for f in (source / "cockpit/internal/worker/imageverify").iterdir()}
    expected = {name for name in files if name.startswith("cockpit/internal/worker/imageverify/")}
    if actual != expected:
        raise Refused("The qualification package contains unreviewed files")
    if len(p["fixtures"]) != 8 or len(set(p["fixtures"])) != 8:
        raise Refused("The eight real fixture identities are missing")
    for item in (p["systemctl"], p["go"]):
        if set(item) != {"path", "sha256"} or not Path(item["path"]).is_absolute():
            raise Refused("The exact worker/tool identity is missing")
        if digest(regular(item["path"], 64 << 20)) != item["sha256"]:
            raise Refused("The worker or system tool identity changed")
    if not re.fullmatch(r"memql[-A-Za-z0-9_.@]*\.service", p["worker"]["unit"]):
        raise Refused("The exact worker unit is missing")
    unit = p["worker"]
    if set(unit) != {"unit", "unitFile"} or set(unit["unitFile"]) != {"path"} or not Path(unit["unitFile"]["path"]).is_absolute():
        raise Refused("The exact worker unit path is missing")
    regular(unit["unitFile"]["path"], 1 << 20)
    return p


def load_guard(plan, source):
    name = "scripts/lib/host_build_guard.py"
    raw = regular(source / name, 2 << 20).read_bytes()
    if hashlib.sha256(raw).hexdigest() != plan["sourceFiles"][name]:
        raise Refused("The shared build guard changed")
    # Execute these verified bytes, never a cached .pyc beside the checkout.
    module = types.ModuleType("host_build_guard")
    exec(compile(raw, str(source / name), "exec"), module.__dict__)
    return module


def systemctl(plan, *args):
    tool = plan["systemctl"]
    if digest(regular(tool["path"], 16 << 20, 0)) != tool["sha256"]:
        raise Refused("The system manager tool changed")
    result = subprocess.run([tool["path"], "--user", *args], capture_output=True, timeout=30,
                            env={k: v for k, v in os.environ.items() if k in ("HOME", "USER", "LOGNAME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS")})
    if result.returncode or len(result.stdout) > 65536 or len(result.stderr) > 65536:
        raise Refused("The worker manager request could not be verified")
    return result.stdout.decode("utf-8")


def loaded_settings_digest(current):
    if current.get("DropInPaths") != "" or current.get("Transient") != "no" or current.get("NeedDaemonReload") != "no":
        raise Refused("The worker has unreviewed drop-ins or pending loaded changes")
    if any(k not in current for k in LOADED_PROPERTIES):
        raise Refused("The loaded worker settings are incomplete")
    values = {k:current[k] for k in LOADED_PROPERTIES}
    # systemd appends outcome metadata to this single command. It changes on
    # stop/start but is not configuration. Refuse an unfamiliar representation.
    value = values["ExecStart"]
    suffix = r"\s*;\s*start_time=\[[^\]]*\]\s*;\s*stop_time=\[[^\]]*\]\s*;\s*pid=[0-9]+\s*;\s*code=[^;{}]*\s*;\s*status=[^;{}]*\s*\}$"
    match = re.search(suffix, value)
    if not value.startswith("{ path=") or not match or "}" in value[:match.start()]:
        raise Refused("The loaded worker command cannot be compared safely")
    values["ExecStart"] = value[:match.start()].rstrip() + " }"
    return hashlib.sha256(json.dumps(values, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def worker_snapshot(plan, prior=None):
    unit = plan["worker"]
    props = ",".join(("Id", "ActiveState", "SubState", "MainPID", "FragmentPath", "ControlGroup", "UnitFileState", "DropInPaths", "Transient", "NeedDaemonReload", *LOADED_PROPERTIES))
    lines = systemctl(plan, "show", unit["unit"], "--property=" + props, "--no-pager").splitlines()
    current = dict(line.split("=", 1) for line in lines if "=" in line)
    if current.get("Id") != unit["unit"] or current.get("FragmentPath") != unit["unitFile"]["path"] or current.get("KillMode") != "control-group":
        raise Refused("The selected worker unit no longer matches")
    unit_hash = digest(regular(unit["unitFile"]["path"], 1 << 20))
    loaded = loaded_settings_digest(current)
    if prior is not None and (unit_hash != prior.get("unitFileSHA256") or loaded != prior.get("loadedSettingsSHA256")):
        raise Refused("The captured worker configuration changed")
    # Never retain Environment or ExecStart values: they may contain secrets.
    result = {k:current[k] for k in ("Id", "ActiveState", "SubState", "MainPID", "FragmentPath", "ControlGroup", "UnitFileState", "KillMode")}
    result["loadedSettingsSHA256"] = loaded
    result["unitFileSHA256"] = unit_hash
    return result


def stopped(plan, prior):
    current = worker_snapshot(plan, prior)
    if current.get("ActiveState") != "inactive" or current.get("MainPID") != "0" or current.get("UnitFileState") != prior.get("UnitFileState") or current.get("loadedSettingsSHA256") != prior.get("loadedSettingsSHA256"):
        raise Refused("The worker is not fully stopped or its settings changed")
    group = prior.get("ControlGroup", "")
    if group:
        if not group.startswith("/user.slice/user-" + str(os.getuid()) + ".slice/") or ".." in group.split("/"):
            raise Refused("The worker control group cannot be identified")
        root = Path("/sys/fs/cgroup") / group.lstrip("/")
        if root.exists():
            for item in root.rglob("cgroup.procs"):
                if regular(item, 65536).read_text().strip():
                    raise Refused("A worker process is still running")


def point_in_time_idle(plan):
    receipt = plan["idleReceipt"]
    value = read_json(receipt["path"])
    if digest(receipt["path"]) != receipt["sha256"] or value.get("workerUnit") != plan["worker"]["unit"] or value.get("ownerUID") != os.getuid() or type(value.get("activeWork")) is not int or value["activeWork"] != 0:
        raise Refused("The owner-reviewed idle evidence does not match this worker")
    age = (dt.datetime.now(dt.timezone.utc) - dt.datetime.fromisoformat(value["observedAt"].replace("Z", "+00:00"))).total_seconds()
    if not 0 <= age <= MAX_SETUP_AGE_SECONDS:
        raise Refused("This setup observation is too old; refresh the owned activity check before stopping")
    # This is provenance, not a lease: a new in-process action can arrive after
    # the observation. Existing local locks cover build/inference only.
    return {"observedAt": value["observedAt"], "activeWork": 0, "sha256": receipt["sha256"], "pointInTimeOnly": True}


def no_worker_descendants(current, _root=Path("/sys/fs/cgroup")):
    group = current.get("ControlGroup", "")
    active = current.get("ActiveState") == "active"
    if not group:
        if active:
            raise Refused("The running worker cgroup is missing")
        return
    if not group.startswith("/user.slice/user-" + str(os.getuid()) + ".slice/") or ".." in group.split("/"):
        raise Refused("The worker cgroup cannot be identified")
    root = _root / group.lstrip("/")
    if not root.is_dir():
        if active:
            raise Refused("The running worker cgroup disappeared")
        return
    allowed = {current.get("MainPID")} if active else set()
    if active and (not str(current.get("MainPID", "")).isdigit() or int(current["MainPID"]) <= 0):
        raise Refused("The running worker identity is missing")
    regular(root / "cgroup.procs", 65536)
    for path in root.rglob("cgroup.procs"):
        pids = set(regular(path, 65536).read_text().split())
        if any(not p.isdigit() for p in pids) or pids - allowed:
            raise Refused("The worker has active child processes; wait for them to finish")


def stop_for_qualification(plan, prior, statepath, state):
    observation = point_in_time_idle(plan)
    current = worker_snapshot(plan, prior)
    if current.get("ActiveState") != prior.get("ActiveState") or current.get("UnitFileState") != prior.get("UnitFileState"):
        raise Refused("The worker state changed during preparation")
    no_worker_descendants(current)
    # Persist before issuing the bounded stop. Neither the observation nor a
    # zero-child snapshot proves no short in-process action can race this stop.
    state["idleObservation"] = observation
    state["stopRequestedAt"] = dt.datetime.now(dt.timezone.utc).isoformat()
    state["phase"] = "stopping_worker"
    save(statepath, state, replace=True)
    print("MemQL: Pause the worker for isolation checks", file=sys.stderr)
    systemctl(plan, "stop", plan["worker"]["unit"])
    stopped(plan, current)
    state["workerStopped"] = True
    save(statepath, state, replace=True)

def run(command, env, cwd, log, guard, fd, record, timeout, verbose=False):
    if verbose:
        print("MemQL diagnostic: " + repr(command), file=sys.stderr)
    child = subprocess.Popen(command, cwd=cwd, env=env, stdout=log, stderr=log,
                             start_new_session=True, pass_fds=(fd,))
    record["processGroup"] = child.pid
    guard.write_marker(guard.capacity_root(), record, replace=True)
    deadline = time.monotonic() + timeout
    try:
        while child.poll() is None:
            guard.disk_preflight(cwd, RUN_FLOOR_MIB)
            if time.monotonic() > deadline or os.fstat(log.fileno()).st_size > LOG_LIMIT:
                raise Refused("The bounded operation stopped; cleanup must be checked")
            time.sleep(0.25)
        if child.returncode or guard.group_alive(child.pid):
            raise Refused("The operation did not settle successfully")
    finally:
        if child.poll() is None or guard.group_alive(child.pid):
            for sig, grace in ((signal.SIGTERM, 5), (signal.SIGKILL, 2)):
                try:
                    os.killpg(child.pid, sig)
                except ProcessLookupError:
                    break
                until = time.monotonic() + grace
                while time.monotonic() < until:
                    child.poll()
                    if not guard.group_alive(child.pid):
                        break
                    time.sleep(0.05)
                if not guard.group_alive(child.pid):
                    break
            if child.poll() is None:
                child.wait(timeout=1)


def validate_result(plan, output):
    result = read_json(output / "qualification-result.json", owner=os.getuid())
    if result.get("schemaVersion") != 1 or result.get("sourceSHA") != plan["sourceSHA"] or result.get("passed") is not True or result.get("cleanupConfirmed") is not True:
        raise Refused("The real fixture did not confirm qualification and cleanup")
    if not re.fullmatch(r"[0-9a-f]{64}", result.get("installationSHA256", "")):
        raise Refused("The fixture installation identity is missing")
    fixtures = result.get("fixtures", [])
    if len(fixtures) != 8 or {v.get("name") for v in fixtures} != set(plan["fixtures"]):
        raise Refused("The eight real fixtures are incomplete")
    for item in fixtures:
        if item.get("result") != "passed" or not re.fullmatch(r"[0-9a-f]{64}", item.get("evidenceSHA256", "")):
            raise Refused("A real fixture did not pass with retained evidence")
        evidence = Path(item.get("evidencePath", ""))
        if not evidence.is_absolute() or ".." in evidence.parts or output not in evidence.parents or digest(regular(evidence, 4 << 20, os.getuid())) != item["evidenceSHA256"]:
            raise Refused("A retained fixture evidence file changed or escaped this attempt")
    for field, name in (("helperSHA256", "memql-imageverify"), ("testBinarySHA256", "imageverify.test")):
        if result.get(field) != digest(regular(output / name, 128 << 20, os.getuid())):
            raise Refused("The fixture used different qualification tools")
    candidate = Path(result.get("candidateAttestationPath", ""))
    if candidate != output / "qualification.json":
        raise Refused("The candidate attestation path is outside this attempt")
    attestation = read_json(candidate, 1 << 20, os.getuid())
    if attestation.get("schemaVersion") != 1 or attestation.get("profile") != "rootless-buildkit-v1" or attestation.get("installationSHA256") != result["installationSHA256"]:
        raise Refused("The candidate attestation names another installation")
    recorded = {(v.get("name"), v.get("result"), v.get("evidenceSHA256")) for v in attestation.get("fixtures", [])}
    expected = {(v["name"], v["result"], v["evidenceSHA256"]) for v in fixtures}
    if len(attestation.get("fixtures", [])) != 8 or recorded != expected:
        raise Refused("The candidate attestation differs from the real fixture receipts")
    return result


def settle(guard, record, statepath, state, plan, prior):
    if state.get("phase") != "cleanup_confirmed" or state.get("result", {}).get("cleanupConfirmed") is not True:
        raise Refused("Confirmed service cleanup is required before worker resumption")
    with guard.host_build_guard(_completed_record=record):
        if guard.read_marker(guard.capacity_root()) != record:
            raise Refused("The retained reservation changed before completion")
        stopped(plan, prior)
        os.unlink(guard.capacity_root() / guard.MARKER_NAME)
        guard.sync_directory(guard.capacity_root())
        if prior.get("ActiveState") == "active":
            systemctl(plan, "start", plan["worker"]["unit"])
            if worker_snapshot(plan, prior).get("ActiveState") != "active":
                raise Refused("Cleanup passed, but worker resumption was not confirmed")
        state["phase"] = "complete"
        save(statepath, state, replace=True)


def main():
    ap = Parser(description=__doc__)
    ap.add_argument("--phase", choices=("prepare", "qualify"), required=True)
    ap.add_argument("--plan", type=Path, required=True)
    ap.add_argument("--source", type=Path, required=True)
    ap.add_argument("--output", type=Path, required=True)
    ap.add_argument("--go", type=Path, required=True)
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()
    changed = False
    state = None
    try:
        if sys.platform != "linux" or os.uname().machine != "x86_64" or os.geteuid() == 0:
            raise Refused("Run this as the reviewed Linux worker account")
        if os.environ.get("MEMQL_HOST_BUILD_LOCK_FD") is not None:
            raise Refused("Start this setup outside another active build reservation")
        if any(not p.is_absolute() for p in (args.plan, args.source, args.output, args.go)):
            raise Refused("Setup paths must be absolute")
        if any(".." in p.parts for p in (args.plan, args.source, args.output, args.go)):
            raise Refused("Setup paths must be canonical")
        plan = load_plan(args.plan, args.source)
        if str(args.go) != plan["go"]["path"]:
            raise Refused("The Go executable differs from the reviewed plan")
        guard = load_guard(plan, args.source)
        version = subprocess.run([str(args.go), "version"], capture_output=True, timeout=10, env={"GOTOOLCHAIN": "local", "GOENV": "off", "PATH": "/usr/bin:/bin"})
        if version.returncode or version.stdout.decode().strip() != "go version " + GO_VERSION + " linux/amd64":
            raise Refused("The reviewed Go toolchain is required")
        args.output.mkdir(mode=0o700, exist_ok=True)
        if args.output.is_symlink() or args.output.stat().st_uid != os.getuid() or args.output.stat().st_mode & 0o077:
            raise Refused("The result directory must be private to this account")
        statepath = args.output / "setup-state.json"
        if args.phase == "prepare":
            if statepath.exists():
                raise Refused("This setup already has a retained result; do not replay it")
            observation = point_in_time_idle(plan)
            prior = worker_snapshot(plan)
            if prior.get("ActiveState") not in ("active", "inactive"):
                raise Refused("The worker is changing state")
            state = {"phase": "preparing", "planSHA256": digest(args.plan), "priorWorker": prior, "sourceSHA": plan["sourceSHA"], "idleObservation": observation, "workerStopped": False}
            record = {"kind": "local-build-v1", "token": secrets.token_hex(24), "pid": os.getpid(), "workspace": str(args.output), "startedAt": time.time()}
        else:
            state = read_json(statepath, owner=os.getuid())
            if state.get("phase") != "prepared" or state.get("planSHA256") != digest(args.plan):
                raise Refused("Only this exact prepared setup can continue")
            record = state["reservation"]
            if guard.read_marker(guard.capacity_root()) != record:
                raise Refused("The retained reservation changed")
            prior = state["priorWorker"]
            for n, expected in state["binaries"].items():
                if digest(regular(args.output / n, 128 << 20, os.getuid())) != expected:
                    raise Refused("A prepared qualification binary changed")
        with guard.host_build_guard(_completed_record=record if args.phase == "qualify" else None) as fd:
            guard.disk_preflight(args.output, MIN_FREE_MIB)
            # Old inference uses its own account lock. The deliberately stopped
            # worker, not this lock or a new marker format, is the crash boundary.
            inf = os.open(guard.capacity_root() / "inference.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
            try:
                if not guard.regular_owned(os.fstat(inf)):
                    raise Refused("The inference reservation cannot be verified")
                fcntl.flock(inf, fcntl.LOCK_EX | fcntl.LOCK_NB)
                if args.phase == "prepare":
                    guard.write_marker(guard.capacity_root(), record)
                    save(statepath, state)
                    changed = True
                else:
                    stop_for_qualification(plan, prior, statepath, state)
                changed = True
                record["pid"] = os.getpid()
                guard.write_marker(guard.capacity_root(), record, replace=True)
                state["phase"] = "building" if args.phase == "prepare" else "qualifying"
                state["reservation"] = record
                save(statepath, state, replace=True)
                env = {k:v for k,v in os.environ.items() if k in ("HOME", "USER", "LOGNAME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS")}
                env.update({"PATH": "/usr/bin:/bin", "GOWORK": "off", "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local", "GOENV": "off", "GOMAXPROCS": "2", "GOMEMLIMIT": "2GiB", "GOFLAGS": "-p=1", "CGO_ENABLED": "0", "GOCACHE": str(args.output / "go-cache"), guard.LOCK_FD_ENV: str(fd), guard.TOKEN_ENV: record["token"]})
                logpath = args.output / (args.phase + ".log")
                with open(logpath, "xb") as log:
                    if args.phase == "prepare":
                        print("MemQL: Build qualification tools", file=sys.stderr)
                        module = args.output / "source"
                        module.mkdir(mode=0o700)
                        for name in plan["sourceFiles"]:
                            if name.startswith("cockpit/"):
                                dest = module / name[len("cockpit/"):]
                                dest.parent.mkdir(parents=True, exist_ok=True)
                                raw = (args.source / name).read_bytes()
                                if hashlib.sha256(raw).hexdigest() != plan["sourceFiles"][name]:
                                    raise Refused("Source changed while preparing the isolated build")
                                dest.write_bytes(raw)
                        (module / "go.mod").write_text("module github.com/znasllc-io/memql/cockpit\n\ngo 1.27.3\n")
                        for command in ([str(args.go), "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", str(args.output / "memql-imageverify"), "./cmd/memql-imageverify"], [str(args.go), "test", "-c", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", str(args.output / "imageverify.test"), "./internal/worker/imageverify"]):
                            run(command, env, module, log, guard, fd, record, 900, args.verbose)
                        state["binaries"] = {n:digest(args.output / n) for n in ("memql-imageverify", "imageverify.test")}
                        state["phase"] = "prepared"
                    else:
                        print("MemQL: Run isolation checks", file=sys.stderr)
                        env.update({"MEMQL_IMAGE_QUALIFICATION": "1", "MEMQL_IMAGE_QUALIFICATION_OUTPUT": str(args.output), "MEMQL_IMAGE_QUALIFICATION_SOURCE": plan["sourceSHA"], "MEMQL_IMAGE_QUALIFICATION_WORKER_UNIT": plan["worker"]["unit"]})
                        run([str(args.output / "imageverify.test"), "-test.run=^" + FIXTURE + "$", "-test.v", "-test.timeout=45m"], env, args.output, log, guard, fd, record, 2760, args.verbose)
                        state["result"] = validate_result(plan, args.output)
                        stopped(plan, prior)
                        state["phase"] = "cleanup_confirmed"
                state["reservation"] = record
                save(statepath, state, replace=True)
            finally:
                os.close(inf)
        if args.phase == "qualify":
            settle(guard, record, statepath, state, plan, prior)
        print(json.dumps({"ok":True,"capability":CAPABILITY,"changed":True,"result":{"phase":state["phase"],"statePath":str(statepath),"workerResumed":state["phase"]=="complete" and prior.get("ActiveState")=="active"},"error":None}))
        return 0
    except (Exception, KeyboardInterrupt) as error:
        # A timed-out stop may have taken effect even without its response.
        # The durable intent is the mutation boundary, not helper return.
        changed = changed or bool(state and state.get("stopRequestedAt"))
        message = str(error) or "Setup interrupted"
        recovery = "Keep the worker stopped; inspect the retained setup state and logs" if state and state.get("stopRequestedAt") else "Preparation did not stop the worker; inspect the retained setup state and logs before retrying"
        print("MemQL: " + message + ". " + recovery + ".", file=sys.stderr)
        print(json.dumps({"ok":False,"capability":CAPABILITY,"changed":changed,"result":{"output":str(args.output)},"error":{"code":5 if changed else 3,"message":message}}))
        return 5 if changed else 3


if __name__ == "__main__":
    sys.exit(main())
