#!/usr/bin/env bash
# Library: docker.sh -- one answer to "can this user drive Docker?".
#
# WHY IT IS A LIBRARY. Two capabilities need the answer: `install.detect`, which
# inventories the machine, and `install.dockerAccess`, which gates the install on
# it. A second copy of the probe is a second opinion about whether the machine is
# ready, and the two would drift in the direction that matters least visibly --
# the inventory saying one thing while the gate says another.
#
# WHY IT IS NOT A BOOLEAN (memql#3549). "Is Docker usable" collapses three
# states that need three different remedies:
#
#   missing  -- no docker binary. Install Docker.
#   stopped  -- the binary is there, the daemon is not answering. Start it.
#   denied   -- the daemon IS running and refuses THIS USER on the socket,
#               and the group file does not list them either. Add them.
#   stale    -- the group file DOES list them, and this process does not carry
#               the group. The fix has already been applied and cannot reach a
#               process that was already running (memql#3554).
#
# `detect.sh` reported all three as `dockerDaemon: false`, and the operator-
# facing reading of that is "the daemon is down" -- which sends someone whose
# daemon is active and healthy to `systemctl start docker`, a command that
# changes nothing and tells them nothing. The three are distinguished here, at
# the only place that has the evidence: the daemon's own error text.
#
# Refs: #3549

# docker_access_state -- prints one of: ok | missing | stopped | denied
#
# THE ORDER IS THE LOGIC. Absence is checked first because a missing binary
# cannot produce an error message to classify. Everything after that reads the
# daemon's refusal, and `permission denied` is the one string Docker is specific
# about -- it names the socket it would not open. Anything else the daemon says
# is reported as `stopped`, which is the honest generalisation: the client could
# not get an answer, and the remedy space is "make the daemon answer".
function docker_access_state() {
    command -v docker &>/dev/null || { printf 'missing\n'; return 0; }

    local out=""
    # Bounded, because a wedged daemon does not fail -- it hangs, and this runs
    # on the path that decides whether to show the operator a form.
    if command -v timeout &>/dev/null; then
        out="$(timeout 15 docker info --format '{{.ServerVersion}}' 2>&1)" && { printf 'ok\n'; return 0; }
    else
        out="$(docker info --format '{{.ServerVersion}}' 2>&1)" && { printf 'ok\n'; return 0; }
    fi

    # Matched case-insensitively on the phrase, not on an exit code: Docker
    # returns 1 for every one of these, so the text is the only signal there is.
    #
    # BASH'S OWN PATTERN MATCH, NOT `grep`. This runs with whatever PATH the
    # caller had -- the install lane's own tests give it one carrying four
    # binaries -- and a missing `grep` does not report an error here, it
    # silently fails to match. The cost of that is precisely the confusion this
    # function exists to remove: `denied` classified as `stopped`, sending
    # someone to restart a daemon that is already running.
    local lower="${out,,}"
    if [[ "$lower" == *"permission denied"* || "$lower" == *"dial unix"*"permission"* ]]; then
        # DENIED SPLITS IN TWO, and the second half is the one that traps people
        # (memql#3554). `usermod -aG docker <user>` writes the group file and
        # cannot touch a process that is already running: supplementary groups
        # are fixed when a process starts and inherited from its parent, so
        # every process in the current login session -- the editor, the terminal
        # it spawns, this script -- keeps the credentials it was born with.
        #
        # Reported as `denied`, that state tells an operator who has ALREADY run
        # the command to run it again. It changes nothing, the gate refuses
        # again, and there is no reading of the message that gets them out.
        if docker_group_pending; then
            printf 'stale\n'
            return 0
        fi
        printf 'denied\n'
        return 0
    fi
    printf 'stopped\n'
}

# docker_group_pending -- 0 when the group file lists this user in `docker` but
# the CURRENT process does not carry it.
#
# `id -nG <user>` re-reads the group database; a bare `id -nG` reports the
# credentials this process is actually running with. The two disagreeing is the
# whole signature of "applied, not yet in effect".
#
# GUARDED ON `id` BEING PRESENT, and answers "no" when it is not. This library
# runs with whatever PATH the caller had; claiming the more specific state
# without the evidence for it would be a confident wrong diagnosis, and the
# less specific one is still true.
function docker_group_pending() {
    # macOS has no `docker` group at all -- Docker Desktop's socket is owned by
    # the logged-in user -- so the whole applied-not-yet-in-effect state cannot
    # arise there, and asking `id` about a group that does not exist would
    # answer "no" for the wrong reason (memql#4295).
    [[ "$(uname -s 2>/dev/null || true)" != "Darwin" ]] || return 1
    command -v id &>/dev/null || return 1
    local user recorded current
    user="$(id -un 2>/dev/null)" || return 1
    [[ -n "$user" ]] || return 1
    recorded=" $(id -nG "$user" 2>/dev/null) "
    current=" $(id -nG 2>/dev/null) "
    [[ "$recorded" == *" docker "* && "$current" != *" docker "* ]]
}

# docker_access_remedy <state> -- the exact command that fixes it, or "".
#
# THE COMMAND, NOT A DESCRIPTION OF ONE. It travels in the capability envelope
# to the wizard, which offers to type it into a terminal for the operator to run
# (memql#3549) -- so it has to be the literal thing, correct as written, with
# this machine's user already substituted in.
#
# `missing` has no single command: installing Docker differs per distribution
# and picking one would be a guess presented as an instruction. It returns "",
# and the caller says where to look instead.
#
# THE REMEDIES ARE PLATFORM-SPECIFIC, and the macOS ones are not cosmetic
# variants (memql#4295). On a Mac there is no systemd, so
# `sudo systemctl start docker` is not a worse suggestion than the right one --
# it is a command that does not exist, offered to an operator whose Docker is
# simply not launched. And there is no `docker` group: Docker Desktop's socket
# is owned by the logged-in user, so `usermod -aG docker` names a group that
# does not exist on a system that has no `usermod` either. Both would have been
# typed into a terminal by the wizard, which is why they are worth splitting
# rather than leaving as an approximation.
function docker_access_remedy() {
    if [[ "$(uname -s 2>/dev/null || true)" == "Darwin" ]]; then
        case "$1" in
            # Docker Desktop is an application, not a service. `open -a` is the
            # supported way to start it and returns as soon as it launches --
            # the daemon takes a few more seconds, which is why the gate is
            # re-run rather than chained.
            stopped|denied) printf 'open -a Docker\n' ;;
            *)              printf '' ;;
        esac
        return
    fi
    case "$1" in
        denied)  printf 'sudo usermod -aG docker %s\n' "$(id -un)" ;;
        # NO COMMAND. Nothing run inside this session can give it a group it did
        # not start with, and offering `usermod` again -- the one command that
        # looks relevant -- is exactly the loop this state exists to break.
        stale)   printf '' ;;
        stopped) printf 'sudo systemctl start docker\n' ;;
        *)       printf '' ;;
    esac
}

# docker_access_explanation <state> -- one sentence an operator can act on.
function docker_access_explanation() {
    if [[ "$(uname -s 2>/dev/null || true)" == "Darwin" ]]; then
        case "$1" in
            ok)      printf 'Docker is running and reachable.\n' ;;
            missing) printf 'Docker is not installed on this machine. Install Docker Desktop for Mac (https://docs.docker.com/desktop/install/mac-install/), then run this again.\n' ;;
            # On macOS these collapse: there is no `docker` group to be outside
            # of, so a socket that refuses is a Desktop that is not running (or
            # still starting) rather than a permissions problem.
            stopped|denied) printf 'Docker Desktop is not running -- start it from Applications (or `open -a Docker`), give it a few seconds to finish starting, then run this again.\n' ;;
            *)       printf 'The state of Docker on this machine could not be determined.\n' ;;
        esac
        return
    fi
    case "$1" in
        ok)      printf 'Docker is running and reachable.\n' ;;
        missing) printf 'Docker is not installed on this machine. Install Docker Engine for your distribution, then run this again.\n' ;;
        stopped) printf 'Docker is installed but its daemon is not answering.\n' ;;
        denied)  printf 'The Docker daemon is running but refuses this user on its socket -- you are not in the "docker" group.\n' ;;
        stale)   printf 'You ARE in the "docker" group now, but this session started before that was true and cannot pick it up -- a process keeps the groups it was born with. Log out and log back in (or reboot), then run this again. Restarting the editor alone is NOT enough: it is started by the desktop session, which has the same old credentials.\n' ;;
        *)       printf 'The state of Docker on this machine could not be determined.\n' ;;
    esac
}
