#!/bin/sh
set -eu

repo=${PARROT_REPO:-asdacap/parrot-coder}
bin_dir=${PARROT_INSTALL_DIR:-"${HOME:?HOME is not set}/.local/bin"}
version=${PARROT_VERSION:-latest}

usage() {
	cat <<'EOF'
Install a Parrot Coder release binary.

Usage: install.sh [--version VERSION] [--bin-dir DIRECTORY]

Environment variables:
  PARROT_VERSION      Release version (for example, 1.2.3; default: latest)
  PARROT_INSTALL_DIR  Installation directory (default: ~/.local/bin)
EOF
}

while [ "$#" -gt 0 ]; do
	case $1 in
		--version)
			[ "$#" -ge 2 ] || { printf '%s\n' 'install.sh: --version requires a value' >&2; exit 2; }
			version=$2
			shift 2
			;;
		--bin-dir)
			[ "$#" -ge 2 ] || { printf '%s\n' 'install.sh: --bin-dir requires a value' >&2; exit 2; }
			bin_dir=$2
			shift 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			printf 'install.sh: unknown option: %s\n' "$1" >&2
			usage >&2
			exit 2
			;;
	esac
done

for command in curl tar; do
	command -v "$command" >/dev/null 2>&1 || {
		printf 'install.sh: required command not found: %s\n' "$command" >&2
		exit 1
	}
done

case $(uname -s) in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) printf 'install.sh: unsupported operating system: %s\n' "$(uname -s)" >&2; exit 1 ;;
esac

case $(uname -m) in
	x86_64|amd64) arch=amd64 ;;
	arm64|aarch64) arch=arm64 ;;
	*) printf 'install.sh: unsupported architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

base_url="https://github.com/$repo/releases"
if [ "$version" = latest ]; then
	latest_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$base_url/latest")
	tag=${latest_url##*/}
	case $tag in
		v*) version=${tag#v} ;;
		*) printf 'install.sh: could not determine the latest release\n' >&2; exit 1 ;;
	esac
else
	version=${version#v}
fi

case $version in
	''|*[!0-9A-Za-z.-]*) printf 'install.sh: invalid version: %s\n' "$version" >&2; exit 1 ;;
esac

archive="parrot-$version-$os-$arch.tar.gz"
download_url="$base_url/download/v$version"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/parrot-install.XXXXXX")
tmp_bin=
cleanup() {
	rm -rf "$tmp_dir"
	[ -z "$tmp_bin" ] || rm -f "$tmp_bin"
}
trap cleanup EXIT HUP INT TERM

printf 'Downloading Parrot Coder %s for %s/%s...\n' "$version" "$os" "$arch"
curl -fL --retry 3 -o "$tmp_dir/$archive" "$download_url/$archive"
curl -fL --retry 3 -o "$tmp_dir/SHA256SUMS" "$download_url/SHA256SUMS"

expected=$(awk -v name="$archive" '$2 == "./" name || $2 == name { print $1; exit }' "$tmp_dir/SHA256SUMS")
[ -n "$expected" ] || { printf 'install.sh: checksum not found for %s\n' "$archive" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp_dir/$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$tmp_dir/$archive" | awk '{print $1}')
else
	printf '%s\n' 'install.sh: sha256sum or shasum is required to verify the download' >&2
	exit 1
fi
[ "$actual" = "$expected" ] || { printf '%s\n' 'install.sh: checksum verification failed' >&2; exit 1; }

tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"
extracted="$tmp_dir/parrot-$version-$os-$arch/parrot"
[ -f "$extracted" ] || { printf '%s\n' 'install.sh: release archive does not contain parrot' >&2; exit 1; }

mkdir -p "$bin_dir"
tmp_bin=$(mktemp "$bin_dir/.parrot.XXXXXX")
cp "$extracted" "$tmp_bin"
chmod 0755 "$tmp_bin"
mv -f "$tmp_bin" "$bin_dir/parrot"
tmp_bin=

printf 'Installed Parrot Coder to %s/parrot\n' "$bin_dir"
case :$PATH: in
	*:"$bin_dir":*) ;;
	*) printf 'Add %s to your PATH, then run: parrot auth login openai\n' "$bin_dir" ;;
esac
