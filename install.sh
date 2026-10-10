#!/bin/sh
# tc installer: downloads the latest release (or $TC_VERSION) for this
# machine from GitHub Releases, checks its sha256 against checksums.txt,
# installs it to ~/.local/bin (or $TC_INSTALL_DIR) and, given an invite,
# runs `tc join '<invite>'`.
#
#   curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/main/install.sh | sh -s -- 'tc://…'
#
# Environment: TC_VERSION (e.g. v0.2.0), TC_INSTALL_DIR, TC_RELEASE_BASE
# (default https://github.com/pranavjayakumar/terminal-chat/releases).
set -eu

say() { printf 'tc-install: %s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "this installer needs '$1'"
}

detect_os() {
	case "$(uname -s)" in
	Darwin) echo darwin ;;
	Linux) echo linux ;;
	*) die "unsupported OS '$(uname -s)' (tc ships for macOS and Linux)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	arm64 | aarch64) echo arm64 ;;
	*) die "unsupported CPU '$(uname -m)' (tc ships for amd64 and arm64)" ;;
	esac
}

# valid_tag accepts vMAJOR.MINOR.PATCH with an optional -suffix of safe characters.
valid_tag() {
	case "$1" in
	v[0-9]*.[0-9]*.[0-9]*) ;;
	*) return 1 ;;
	esac
	case "$1" in
	*[!A-Za-z0-9.-]*) return 1 ;;
	esac
	return 0
}

latest_tag() {
	# /releases/latest redirects to /releases/tag/<tag>; no API, no rate limit.
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$1/latest") ||
		die "can't reach $1 (offline, or no releases yet?)"
	case "$url" in
	*/tag/*) ;;
	*) die "unexpected answer from $1/latest: $url" ;;
	esac
	printf '%s\n' "${url##*/}"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "need sha256sum or shasum to verify the download"
	fi
}

main() {
	need curl
	need tar
	need awk
	need uname

	invite=""
	if [ "$#" -gt 1 ]; then
		die "usage: install.sh ['tc://invite']"
	fi
	if [ "$#" -eq 1 ]; then
		invite=$1
		case "$invite" in
		tc://*) ;;
		*) die "the argument should be an invite starting with tc:// (got something else)" ;;
		esac
	fi

	base=${TC_RELEASE_BASE:-https://github.com/pranavjayakumar/terminal-chat/releases}
	base=${base%/}
	dir=${TC_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}
	os=$(detect_os)
	arch=$(detect_arch)

	tag=${TC_VERSION:-}
	if [ -z "$tag" ]; then
		tag=$(latest_tag "$base")
	fi
	case "$tag" in
	v*) ;;
	*) tag="v$tag" ;;
	esac
	valid_tag "$tag" || die "bad version '$tag'"

	asset="tc_${os}_${arch}.tar.gz"
	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t tc-install)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT TERM

	say "downloading tc $tag for $os/$arch"
	curl -fsSL -o "$tmp/checksums.txt" "$base/download/$tag/checksums.txt" ||
		die "can't download checksums.txt for $tag"
	curl -fsSL -o "$tmp/$asset" "$base/download/$tag/$asset" ||
		die "can't download $asset for $tag"

	want=$(awk -v f="$asset" '$2 == f || $2 == "*" f {print $1; exit}' "$tmp/checksums.txt")
	[ -n "$want" ] || die "checksums.txt has no entry for $asset"
	got=$(sha256_of "$tmp/$asset")
	if [ "$want" != "$got" ]; then
		die "checksum mismatch for $asset (expected $want, got $got); not installing"
	fi

	mkdir -p "$tmp/x"
	tar -xzf "$tmp/$asset" -C "$tmp/x" tc || die "can't unpack $asset"
	[ -f "$tmp/x/tc" ] || die "$asset has no tc binary"

	mkdir -p "$dir" || die "can't create $dir"
	# Copy next to the target, then rename: atomic, and a running tc keeps working.
	cp "$tmp/x/tc" "$dir/.tc.new.$$" || die "can't write to $dir"
	chmod 755 "$dir/.tc.new.$$"
	mv -f "$dir/.tc.new.$$" "$dir/tc" || {
		rm -f "$dir/.tc.new.$$"
		die "can't install to $dir/tc"
	}
	say "installed $dir/tc ($tag)"

	case ":${PATH:-}:" in
	*":$dir:"*)
		found=$(command -v tc 2>/dev/null || true)
		if [ -n "$found" ] && [ "$found" != "$dir/tc" ]; then
			say "note: $found comes first on your PATH; remove it or put $dir before it"
		fi
		;;
	*)
		rc="your shell's startup file"
		case "${SHELL:-}" in
		*/zsh) rc="$HOME/.zshrc" ;;
		*/bash) rc="$HOME/.bashrc" ;;
		esac
		say "$dir is not on your PATH. Add this line to $rc, then open a new terminal:"
		# shellcheck disable=SC2016 # $PATH is meant literally
		printf '\n  export PATH="%s:$PATH"\n\n' "$dir" >&2
		;;
	esac

	if [ -n "$invite" ]; then
		# stdin is this script when piped into sh; give tc the terminal instead
		# (or nothing, so it uses defaults without prompting).
		if [ -t 1 ] && (: </dev/tty) 2>/dev/null; then
			"$dir/tc" join "$invite" </dev/tty
		else
			"$dir/tc" join "$invite" </dev/null
		fi
	else
		say "next: $dir/tc join 'tc://…' with the invite your admin sent"
	fi
}

# Everything runs from here, so a partly downloaded script does nothing.
main "$@"
