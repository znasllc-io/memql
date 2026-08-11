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
#               which is group membership and nothing else.
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
        printf 'denied\n'
        return 0
    fi
    printf 'stopped\n'
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
function docker_access_remedy() {
    case "$1" in
        denied)  printf 'sudo usermod -aG docker %s\n' "$(id -un)" ;;
        stopped) printf 'sudo systemctl start docker\n' ;;
        *)       printf '' ;;
    esac
}

# docker_access_explanation <state> -- one sentence an operator can act on.
function docker_access_explanation() {
    case "$1" in
        ok)      printf 'Docker is running and reachable.\n' ;;
        missing) printf 'Docker is not installed on this machine. Install Docker Engine for your distribution, then run this again.\n' ;;
        stopped) printf 'Docker is installed but its daemon is not answering.\n' ;;
        denied)  printf 'The Docker daemon is running but refuses this user on its socket -- you are not in the "docker" group.\n' ;;
        *)       printf 'The state of Docker on this machine could not be determined.\n' ;;
    esac
}
