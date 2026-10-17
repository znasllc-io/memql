#!/bin/sh
# Source from the attempt checkout. Native workers can have a read-only HOME;
# the runner owns and removes these caches with this exact checkout.
export GOTOOLCHAIN=go1.26.9 GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=2 GOMEMLIMIT=1GiB
export GOPATH="$PWD/.memql-native/go"
export GOCACHE="$PWD/.memql-native/go-build"
export PATH="$GOPATH/bin:$PATH"
mkdir -p "$GOPATH" "$GOCACHE"
