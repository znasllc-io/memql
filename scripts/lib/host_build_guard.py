#!/usr/bin/env python3
"""Shared local/Fleet build admission, using the OS account's capacity lock.

host_build_guard() acquires the same flock as Fleet, or adopts a checked
inherited descriptor. It never unlinks/truncates pipeline.lock. The supervisor
alone records local-build.json; cleanup callers only reserve capacity.

An interrupted marker is deliberately not cleared based on PID disappearance:
Docker daemon work can outlive its client. --inspect reports the retained
operation. Reconcile its process group, Docker work and imported/desired images
before an operator removes that exact marker under the shared exclusive lock.
"""

import argparse
import contextlib
import datetime
import fcntl
import hashlib
import json
import os
import pathlib
import pwd
import re
import secrets
import signal
import stat
import subprocess
import sys
import time

LOCK_FD_ENV = "MEMQL_HOST_BUILD_LOCK_FD"
TOKEN_ENV = "MEMQL_HOST_BUILD_LOCK_TOKEN"
DEFAULT_MIN_FREE_MIB = 20480
MARKER_NAME = "local-build.json"
CANCEL_GRACE_SECONDS = 5
# The Cockpit's local model call fence, read by the rule its own readers apply
# (cockpit/internal/worker/capacity/host_inference.go): a version 2 record names
# when it ends; one that names none ends INFERENCE_FENCE_BOUND_SECONDS after its
# file was last written (the Cockpit's InferenceFenceBound).
INFERENCE_NAME = "inference-active.json"
INFERENCE_RECORD_VERSION = 2
INFERENCE_FENCE_BOUND_SECONDS = 30 * 60
RFC3339 = re.compile(r"(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})([.,]\d+)?(Z|[+-]\d{2}:\d{2})", re.ASCII)


class GuardError(RuntimeError):
    pass


def capacity_root():
    # HOME/MEMQL_HOME vary per enrollment; Fleet uses the OS account instead.
    home = pwd.getpwuid(os.getuid()).pw_dir
    if not home or not os.path.isabs(home):
        raise GuardError("Cannot resolve this account's build capacity directory")
    return pathlib.Path(home) / ".memql" / "worker-capacity"


def regular_owned(info):
    return stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_nlink == 1


def read_marker(root):
    try:
        fd = os.open(root / MARKER_NAME, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return None
    try:
        info = os.fstat(fd)
        if not regular_owned(info) or not 0 < info.st_size <= 8192:
            raise GuardError("The retained local build record cannot be verified")
        marker = json.loads(os.read(fd, 8193))
        if not isinstance(marker, dict) or marker.get("kind") != "local-build-v1" or not isinstance(marker.get("token"), str) or not marker["token"]:
            raise GuardError("The retained local build record cannot be verified")
        return marker
    finally:
        os.close(fd)


def rfc3339_seconds(text):
    """The epoch seconds of an RFC 3339 time as the Cockpit writes it, else None.

    The Cockpit writes Go's RFC3339Nano in UTC. Text no Cockpit writes can still
    be read differently here than by Go's parser; either answer is then a time
    the record itself names, or the file's age when neither reads one.
    """
    match = RFC3339.fullmatch(text) if isinstance(text, str) else None
    if match is None:
        return None
    year, month, day, hour, minute, second = (int(part) for part in match.groups()[:6])
    try:
        when = datetime.datetime(year, month, day, hour, minute, second, tzinfo=datetime.timezone.utc).timestamp()
    except ValueError:
        return None
    zone = match.group(8)
    if zone != "Z":
        hours, minutes = int(zone[1:3]), int(zone[4:6])
        if hours > 23 or minutes > 59:
            return None
        when -= (-1 if zone[0] == "-" else 1) * (hours * 3600 + minutes * 60)
    fraction = match.group(7)
    return when + (float("0." + fraction[1:]) if fraction else 0)


def inference_fence_end(root):
    """When the retained model call fence ends, in epoch seconds; None for no fence.

    A record that is not a regular file is never interpreted, as the Cockpit
    never writes one: GuardError, whose recovery is an operator's. A record
    that names no readable end (an older Cockpit's, or a write that did not
    complete) ends the bound after its file was last written.
    """
    path = root / INFERENCE_NAME
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return None
    if not stat.S_ISREG(info.st_mode):
        raise GuardError("An inference record that cannot be verified needs reconciliation before a local build can start")
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        opened = os.fstat(fd)
        if (opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino) or not stat.S_ISREG(opened.st_mode):
            raise GuardError("An inference record that cannot be verified needs reconciliation before a local build can start")
        data = os.read(fd, 4096)
    finally:
        os.close(fd)
    try:
        record = json.loads(data)
    except (ValueError, RecursionError):
        record = None
    if isinstance(record, dict) and record.get("version") == INFERENCE_RECORD_VERSION:
        until = rfc3339_seconds(record.get("until"))
        if until is not None:
            return until
    return opened.st_mtime + INFERENCE_FENCE_BOUND_SECONDS


def recovery_message():
    return ("An interrupted local build needs reconciliation. Inspect it with "
            "python3 scripts/lib/host_build_guard.py --inspect; verify its processes, "
            "Docker work and imported images, then use the documented digest-bound --recover command")


def marker_digest(marker):
    return hashlib.sha256(json.dumps(marker, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


@contextlib.contextmanager
def host_build_guard(inherited_fd=None, *, _completed_record=None):
    root = capacity_root()
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = root / "pipeline.lock"
    fd = None
    probe = None
    inherited = inherited_fd if inherited_fd is not None else os.environ.get(LOCK_FD_ENV)
    try:
        probe = os.open(path, os.O_CREAT | os.O_RDWR | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
        canonical = os.fstat(probe)
        if not regular_owned(canonical):
            raise GuardError("Build capacity lock is not an owned regular file")
        if inherited is not None:
            if not str(inherited).isdigit():
                raise GuardError("Invalid inherited build reservation")
            fd = os.dup(int(inherited))
            current = os.fstat(fd)
            if not regular_owned(current) or (current.st_dev, current.st_ino) != (canonical.st_dev, canonical.st_ino):
                raise GuardError("Inherited build reservation belongs to another file")
        else:
            fd, probe = probe, None
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise GuardError("This machine is already building or running a Fleet workbench; retry when it finishes") from error
        if os.fstat(fd).st_size:
            raise GuardError("An interrupted Fleet pipeline needs reconciliation before a local build can start")
        # Inference may outlive the worker process that held the shared lock, so
        # its record fences builds until the time it names: the call's own
        # deadline, at which its request is cut off. Before then a PID's
        # disappearance proves nothing. Past it the record reads as absent, as
        # for the Cockpit's readers. It is another program's record, so this
        # helper leaves it for the Cockpit's next exclusive reader to remove.
        end = inference_fence_end(root)
        if end is not None and time.time() <= end:
            ends = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(end))
            raise GuardError(f"A local model call may still be running on this machine (until {ends} at the latest); retry after it ends")
        for path in root.glob("workbench-*.lock"):
            info = path.lstat()
            if not regular_owned(info) or info.st_size:
                raise GuardError("An interrupted Fleet workbench needs reconciliation before a local build can start")
        marker = read_marker(root)
        if marker is not None:
            token = os.environ.get(TOKEN_ENV, "")
            completing = inherited is None and _completed_record is not None and marker == _completed_record
            if not completing and (inherited is None or not isinstance(marker, dict) or marker.get("kind") != "local-build-v1"
                    or not token or not secrets.compare_digest(str(marker.get("token", "")), token)):
                raise GuardError(recovery_message())
        yield fd
    except (OSError, ValueError) as error:
        raise GuardError("Cannot verify this machine's build reservation: " + str(error)) from error
    finally:
        # Closing a duplicate does not unlock the parent's shared open-file
        # description. LOCK_UN here would prematurely release its reservation.
        if probe is not None:
            os.close(probe)
        if fd is not None:
            os.close(fd)


def disk_preflight(workspace, minimum_mib=DEFAULT_MIN_FREE_MIB):
    if not isinstance(minimum_mib, int) or minimum_mib < 1024:
        raise GuardError("Minimum free disk must be at least 1024 MiB")
    try:
        info = os.statvfs(workspace)
    except OSError as error:
        raise GuardError("Cannot measure available disk for the build workspace") from error
    available = info.f_bavail * info.f_frsize
    if available < minimum_mib * 1024 * 1024:
        raise GuardError(f"Build stopped before starting: {available // (1024 * 1024)} MiB available; {minimum_mib} MiB required. Free unused cache space and retry")
    return available


def sync_directory(root):
    fd = os.open(root, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_marker(root, record, *, replace=False):
    target = root / (".local-build-" + secrets.token_hex(12) if replace else MARKER_NAME)
    fd = os.open(target, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
    try:
        data = json.dumps(record, sort_keys=True).encode()
        if os.write(fd, data) != len(data):
            raise GuardError("Could not persist the local build reservation")
        os.fsync(fd)
    finally:
        os.close(fd)
    if replace:
        os.replace(target, root / MARKER_NAME)
    sync_directory(root)


def group_alive(pgid):
    try:
        os.killpg(pgid, 0)
        return True
    except ProcessLookupError:
        return False


def collect_child(child, interrupted):
    deadline = None
    killed = False
    while True:
        try:
            return child.communicate(timeout=0.2)[0]
        except subprocess.TimeoutExpired:
            if not interrupted:
                continue
            if deadline is None:
                deadline = time.monotonic() + CANCEL_GRACE_SECONDS
            elif time.monotonic() >= deadline:
                if killed:
                    # An escaped descendant can retain stdout. It cannot make
                    # cancellation unbounded; the durable marker stays fenced.
                    child.stdout.close()
                    raise GuardError("Local build cancellation could not be confirmed")
                if child.poll() is None:
                    try:
                        os.killpg(child.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                killed = True
                deadline = time.monotonic() + 2


def recover(args):
    if args.confirm != "reconciled-local-build" or not args.record:
        raise GuardError("Recovery requires the inspected record digest and --confirm=reconciled-local-build after Docker and image reconciliation")
    if os.environ.get(LOCK_FD_ENV) is not None:
        raise GuardError("Recovery requires a fresh reservation, outside any active build")
    root = capacity_root()
    record = read_marker(root)
    if record is None or not secrets.compare_digest(marker_digest(record), args.record):
        raise GuardError("The retained build does not match the inspected record")
    with host_build_guard(_completed_record=record):
        if read_marker(root) != record:
            raise GuardError("The retained build changed; inspect it again")
        pid = record.get("pid")
        if not isinstance(pid, int) or isinstance(pid, bool) or pid <= 1:
            raise GuardError("The retained supervisor identity cannot be verified")
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            pass
        else:
            raise GuardError("The recorded build supervisor is still present")
        pgid = record.get("processGroup")
        if pgid is not None:
            if not isinstance(pgid, int) or isinstance(pgid, bool) or pgid <= 1 or group_alive(pgid):
                raise GuardError("The recorded build process group has not finished")
        # The explicit confirmation attests daemon-side reconciliation. Process
        # disappearance alone is never evidence that Docker work has stopped.
        os.unlink(root / MARKER_NAME)
        sync_directory(root)
    return {"ok": True, "capability": "host-build.recover", "changed": True,
            "result": {"record": args.record}, "error": None}


def supervise(args):
    result = None
    changed = False
    marker_written = False
    child = None
    interrupted = []
    previous_handlers = {}
    try:
        with host_build_guard() as fd:
            disk_preflight(args.workspace, args.min_free_disk_mib)
            root = capacity_root()
            token = secrets.token_hex(24)
            record = {"kind": "local-build-v1", "token": token, "pid": os.getpid(),
                      "workspace": os.path.abspath(args.workspace), "startedAt": time.time()}
            write_marker(root, record)
            marker_written = True
            env = dict(os.environ, **{LOCK_FD_ENV: str(fd), TOKEN_ENV: token})
            for sig in (signal.SIGINT, signal.SIGTERM):
                previous_handlers[sig] = signal.getsignal(sig)
                def forward(received, _frame):
                    interrupted.append(received)
                    if child is not None and child.poll() is None:
                        try:
                            os.killpg(child.pid, received)
                        except ProcessLookupError:
                            pass
                signal.signal(sig, forward)
            child = subprocess.Popen(args.command, env=env, stdout=subprocess.PIPE,
                                     start_new_session=True, pass_fds=(fd,))
            for received in interrupted:
                try:
                    os.killpg(child.pid, received)
                except ProcessLookupError:
                    pass
            # Child PID == its process-group ID. Persist it before trusting any
            # outcome; replacing only our separate marker preserves lock inode.
            record["processGroup"] = child.pid
            write_marker(root, record, replace=True)
            output = collect_child(child, interrupted)
            result = json.loads(output)
            if not isinstance(result, dict) or result.get("capability") != "k3d.dev":
                raise GuardError("Local build returned an invalid result")
            changed = bool(result.get("changed"))
            if child.returncode != 0 or result.get("ok") is not True or interrupted or group_alive(child.pid):
                raise GuardError(recovery_message())
            # Exact successful command + no surviving group is the only
            # automatic clearing path. Errors/signals/unknown output retain it.
        # Drop this open-file description and reacquire through a fresh one.
        # A detached descendant retaining the inherited fd keeps EX locked and
        # refuses completion. The durable marker fences Fleet during handoff.
        with host_build_guard(_completed_record=record):
            if read_marker(root) != record:
                raise GuardError("Local build reservation changed; recovery is required")
            os.unlink(root / MARKER_NAME)
            sync_directory(root)
            print(json.dumps(result, separators=(",", ":")))
            return 0
    except (GuardError, OSError, ValueError) as error:
        if child is not None and child.poll() is None:
            # Do not let a failed supervisor strand a known running child.
            # Its inherited lock and durable marker remain until it terminates.
            try:
                os.killpg(child.pid, signal.SIGTERM)
                child.wait(timeout=5)
            except (ProcessLookupError, subprocess.TimeoutExpired):
                pass
        message = str(error)
        if marker_written and "reconciliation" not in message:
            message += ". " + recovery_message()
        print(message, file=sys.stderr)
        print(json.dumps({"ok": False, "capability": "k3d.dev", "changed": changed or marker_written,
                          "result": result.get("result", {}) if isinstance(result, dict) else {},
                          "error": {"code": 3 if not marker_written else 5, "message": message}}))
        return 3 if not marker_written else 5
    finally:
        for sig, handler in previous_handlers.items():
            signal.signal(sig, handler)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inspect", action="store_true")
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--recover", action="store_true")
    parser.add_argument("--record")
    parser.add_argument("--confirm")
    parser.add_argument("--workspace", default=os.getcwd())
    parser.add_argument("--min-free-disk-mib", type=int, default=DEFAULT_MIN_FREE_MIB)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.inspect:
        try:
            marker = read_marker(capacity_root())
            digest = marker_digest(marker) if marker else None
            if marker:
                marker = {key: value for key, value in marker.items() if key != "token"}
            print(json.dumps({"capacityDirectory": str(capacity_root()), "record": digest, "localBuild": marker}))
            return 0
        except (GuardError, OSError, ValueError) as error:
            print(str(error), file=sys.stderr)
            return 3
    if args.recover:
        try:
            print(json.dumps(recover(args)))
            return 0
        except (GuardError, OSError, ValueError) as error:
            print(str(error), file=sys.stderr)
            print(json.dumps({"ok": False, "capability": "host-build.recover", "changed": False,
                              "result": {}, "error": {"code": 3, "message": str(error)}}))
            return 3
    if args.check:
        try:
            if not os.environ.get(LOCK_FD_ENV) or not os.environ.get(TOKEN_ENV):
                raise GuardError("Local build has no inherited reservation")
            with host_build_guard():
                if read_marker(capacity_root()) is None:
                    raise GuardError("Local build has no durable reservation")
                disk_preflight(args.workspace, args.min_free_disk_mib)
            return 0
        except (GuardError, OSError, ValueError) as error:
            print(str(error), file=sys.stderr)
            return 3
    if args.command[:1] == ["--"]:
        args.command = args.command[1:]
    if not args.command:
        parser.error("a command or --inspect/--check is required")
    return supervise(args)


if __name__ == "__main__":
    sys.exit(main())
