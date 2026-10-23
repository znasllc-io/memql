#!/bin/sh
# Source from the attempt checkout. Native workers can have a read-only HOME;
# the runner owns and removes these caches with this exact checkout.
# Services do not read interactive shell profiles. Keep their configured tools
# first, then discover existing Go/Homebrew installations without installing or
# changing anything in the owner's home.
export PATH="$PATH:$HOME/.local/go/bin:/usr/local/go/bin:/opt/homebrew/bin:/usr/local/bin"
if ! command -v go >/dev/null 2>&1; then
    printf '%s\n' 'Native pipeline checks require an installed Go toolchain.' >&2
    return 1
fi
export GOTOOLCHAIN=go1.26.10 GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=2 GOMEMLIMIT=1GiB
export GOPATH="$PWD/.memql-native/go"
export GOCACHE="$PWD/.memql-native/go-build"
export PATH="$GOPATH/bin:$PATH"
mkdir -p "$GOPATH" "$GOCACHE"
