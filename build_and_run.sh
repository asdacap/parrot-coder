#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
output=${OUTPUT:-"$root/bin/parrot"}

ulimit -c unlimited
GOTRACEBACK=${GOTRACEBACK:-crash}
export GOTRACEBACK

OUTPUT="$output" "$root/build.sh"

exec "$output" "$@"
