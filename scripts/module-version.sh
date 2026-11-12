#!/bin/sh
# Print the Go module version of a commit of github.com/discobox-ai/vm (HEAD
# unless named) as a module that requires it names it: its v* tag, or the
# pseudo-version Go derives for it. Asked of the commit's origin, not the
# module proxy, so a commit just pushed resolves; only the version is read,
# so the checksum database is not asked.
set -eu
rev=${1:-$(git rev-parse HEAD)}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
cd "$dir"
GOFLAGS=-mod=mod GOPROXY=direct GOSUMDB=off go list -m -f '{{.Version}}' "github.com/discobox-ai/vm@$rev"
