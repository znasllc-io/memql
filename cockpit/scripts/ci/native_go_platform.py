"""Prepare the selected native Go toolchain without retrying build or test work."""

import argparse
import json
import os
import signal
import subprocess
import sys
import time


ATTEMPTS = 3
ATTEMPT_TIMEOUT_SECONDS = 120
RETRY_DELAY_SECONDS = 2


class PreparationTimeout(Exception):
    pass


def read_platform():
    # Keep every child in the native worker's process group. Its teardown must
    # still reach them if cancellation kills this helper before its handler runs.
    process = subprocess.Popen(["go", "env", "GOOS", "GOARCH"], stdout=subprocess.PIPE)
    completed = False
    try:
        output, _ = process.communicate(timeout=ATTEMPT_TIMEOUT_SECONDS)
        completed = True
    except subprocess.TimeoutExpired:
        # No retry: a descendant may still hold a pipe. Return to the
        # worker promptly so its normal group teardown ends the whole step.
        raise PreparationTimeout from None
    finally:
        if not completed:
            if process.poll() is None:
                process.kill()
            # Do not communicate or use Popen's context manager here: either
            # could wait indefinitely on an inherited pipe or the launcher.
            process.stdout.close()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                raise PreparationTimeout from None
        else:
            process.stdout.close()
    return output if process.returncode == 0 else None


def prepare_platform(goos, goarch):
    expected = f"{goos}\n{goarch}\n".encode()
    for attempt in range(1, ATTEMPTS + 1):
        output = read_platform()
        if output is not None:
            # A successful command must match exactly; wrong/partial platforms
            # are configuration errors, not another reason to download again.
            if output != expected:
                print(f"This check requires the {goos}/{goarch} Go platform.", file=sys.stderr)
                return False
            if attempt > 1:
                print(f"Go toolchain preparation succeeded on attempt {attempt} of {ATTEMPTS}.", file=sys.stderr)
            return True
        if attempt < ATTEMPTS:
            print(f"Go toolchain preparation failed on attempt {attempt} of {ATTEMPTS}; retrying.", file=sys.stderr)
            time.sleep(RETRY_DELAY_SECONDS)
    print(f"Go toolchain preparation failed after {ATTEMPTS} attempts.", file=sys.stderr)
    return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--os", required=True, choices=["darwin", "linux"])
    parser.add_argument("--arch", required=True, choices=["arm64", "amd64"])
    args = parser.parse_args()
    def cancel(signum, _frame):
        raise SystemExit(128 + signum)
    signal.signal(signal.SIGTERM, cancel)
    signal.signal(signal.SIGINT, cancel)
    try:
        ok = prepare_platform(args.os, args.arch)
    except PreparationTimeout:
        print("Go toolchain preparation timed out; this check has stopped.", file=sys.stderr)
        ok = False
    except OSError:
        print("Go toolchain preparation could not start.", file=sys.stderr)
        ok = False
    print(json.dumps({"ok": ok, "platform": f"{args.os}/{args.arch}"}))
    return 0 if ok else 5


if __name__ == "__main__":
    sys.exit(main())
