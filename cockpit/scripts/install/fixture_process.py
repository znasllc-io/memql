"""Bound offline installer fixtures by the enclosing suite, including children."""

import argparse
import os
import shlex
import signal
import subprocess
import sys
import time

DEADLINE_ENV = "MEMQL_INSTALLER_TEST_DEADLINE_UNIX_MS"
STANDALONE_SECONDS = 600
CLEANUP_SECONDS = 5


class FixtureProcesses:
    def __init__(self, deadline, cleanup_deadline=None):
        self.deadline = deadline
        self.cleanup_deadline = cleanup_deadline

    @classmethod
    def from_environment(cls):
        # Convert once to a monotonic deadline. Each command spends the same
        # suite budget; a retry or a new test must not start another ten minutes.
        inherited = os.environ.get(DEADLINE_ENV)
        remaining = (float(inherited) / 1000 - time.time()
                     if inherited else STANDALONE_SECONDS)
        # Leave the Go wrapper time to receive timeout diagnostics after killing
        # descendants and closing HTTP fixtures, before it ends Python itself.
        # On a short budget, a tenth can be less than HTTPServer's shutdown poll;
        # reserve half instead. Normal native lanes still reserve only 5 seconds.
        now = time.monotonic()
        remaining = max(remaining, 0)
        reserve = min(remaining / 2, CLEANUP_SECONDS)
        return cls(now + remaining - reserve, now + remaining)

    def remaining(self):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise AssertionError("Installer fixture shared suite deadline exhausted")
        return remaining

    def cleanup_limit(self):
        limit = time.monotonic() + CLEANUP_SECONDS
        return min(limit, self.cleanup_deadline) if self.cleanup_deadline is not None else limit

    @staticmethod
    def cleanup_remaining(deadline):
        return max(0, deadline - time.monotonic())

    @staticmethod
    def kill_group(proc, *, deadline=None):
        # The shell can have exited while a logger, curl or grandchild still
        # owns the capture pipe. Kill the group even when proc.poll() is done.
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        except PermissionError as denied:
            # macOS returns EPERM for a group containing only zombies. It can
            # also report terminal EOF while a live orphan still holds a slave,
            # so neither EOF nor the leader's exit justifies ignoring denial.
            # Inspect only PGIDs/states, never unrelated processes' arguments.
            if deadline is None:
                deadline = time.monotonic() + CLEANUP_SECONDS
            try:
                result = subprocess.run(["ps", "-axo", "pgid=,stat="],
                                        capture_output=True, text=True, check=True,
                                        timeout=FixtureProcesses.cleanup_remaining(deadline))
                for line in result.stdout.splitlines():
                    group, state = line.split()
                    if int(group) == proc.pid and not state.startswith("Z"):
                        raise denied
            except (OSError, ValueError, subprocess.SubprocessError) as inspection:
                if inspection is denied:
                    raise
                raise denied from inspection

    def run(self, command, *, env=None, cwd=None, input=None):
        self.remaining()  # Do not start another fixture after budget exhaustion.
        with subprocess.Popen(command, env=env, cwd=cwd, start_new_session=True,
                              stdin=subprocess.PIPE if input is not None else None,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              text=True) as proc:
            try:
                stdout, stderr = proc.communicate(input=input, timeout=self.remaining())
            except (subprocess.TimeoutExpired, AssertionError):
                cleanup = self.cleanup_limit()
                self.kill_group(proc, deadline=cleanup)
                stdout, stderr = proc.communicate(timeout=self.cleanup_remaining(cleanup))
                raise AssertionError(
                    "Installer fixture exceeded shared suite deadline: " + shlex.join(command)
                    + "\nstdout:\n" + stdout + "\nstderr:\n" + stderr) from None
            except BaseException:
                cleanup = self.cleanup_limit()
                self.kill_group(proc, deadline=cleanup)
                proc.communicate(timeout=self.cleanup_remaining(cleanup))
                raise
        return subprocess.CompletedProcess(command, proc.returncode, stdout, stderr)


SUITE_PROCESSES = FixtureProcesses.from_environment()


def main(argv=None):
    """Run a shell suite with the Python fixtures' deadline and tree cleanup."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--program", required=True)
    parser.add_argument("--script", required=True)
    args = parser.parse_args(argv)
    try:
        result = SUITE_PROCESSES.run([args.program, args.script])
    except AssertionError as error:
        # run() includes captured output after terminating the entire group,
        # even when its leader exited before a pipe-holding descendant.
        print(error, file=sys.stderr)
        return 1
    sys.stdout.write(result.stdout)
    sys.stderr.write(result.stderr)
    return result.returncode


if __name__ == "__main__":
    sys.exit(main())
