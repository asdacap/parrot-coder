#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
output=${OUTPUT:-"$root/bin/parrot"}
version=${VERSION:-}

if [ -z "$version" ]; then
	tag=$(git -C "$root" describe --tags --exact-match 2>/dev/null || true)
	if [ -n "$tag" ]; then
		version=${tag#v}
	else
		version=0.0.0-dev
	fi
fi

commit=$(git -C "$root" rev-parse --short=12 HEAD 2>/dev/null || printf '%s' unknown)
date=$(git -C "$root" show -s --format=%cI HEAD 2>/dev/null || printf '%s' unknown)

ldflags="-X main.version=$version -X main.commit=$commit -X main.date=$date"
if [ "${STRIP:-1}" = "1" ]; then
	ldflags="-s -w $ldflags"
fi

mkdir -p "$(dirname -- "$output")"
CGO_ENABLED=${CGO_ENABLED:-0} go build \
	-trimpath \
	-ldflags "$ldflags" \
	-o "$output" \
	"$root/cmd/parrot"

printf 'built %s\n' "$output"
