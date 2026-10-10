#!/bin/sh
# tc installer: downloads the latest release (or $TC_VERSION) for this
# machine from GitHub Releases, checks the maintainer's signature on
# checksums.txt (ssh-keygen -Y verify) and the download's sha256 against it,
# installs it to ~/.local/bin (or $TC_INSTALL_DIR) and, given an invite,
# runs `tc join` with it in $TC_INVITE (not argv, which ps shows to everyone).
#
#   curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/main/install.sh | sh -s -- 'tc://…'
#
# Environment: TC_VERSION (e.g. v0.2.0), TC_INSTALL_DIR, TC_RELEASE_BASE
# (default https://github.com/pranavjayakumar/terminal-chat/releases; must be
# https, or http only to localhost/127.0.0.1/[::1]).
# TC_INSECURE_NO_SIG=1 skips the signature check. That is dangerous: anyone
# who can change the release (or its download) can then run code as you.
set -eu

# Keys allowed to sign releases, in ssh-keygen allowed_signers format. Must
# equal AllowedSigners in internal/release/signers.go (a test checks).
ALLOWED_SIGNERS='tc-release namespaces="tc-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIC1SfY3aK2Wa29kJtVWVGatvoRjOd4rQPWq6d53vWy7D'

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

# fetch is curl restricted to https, for redirects too, with TLS 1.2 or
# newer; -q (first) ignores ~/.curlrc, which could otherwise add -k or a proxy.
# PROTO is =http,https only for a loopback TC_RELEASE_BASE (tests).
PROTO='=https'
fetch() {
	curl -q --proto "$PROTO" --proto-redir "$PROTO" --tlsv1.2 "$@"
}

# check_base accepts https bases, and http only to a loopback host. No
# user info: "http://localhost:1@evil/" is a request to evil.
check_base() {
	case "$1" in
	*@*) die "TC_RELEASE_BASE must not contain '@'" ;;
	https://?*) ;;
	http://localhost | http://localhost[:/]* | http://127.0.0.1 | http://127.0.0.1[:/]* | "http://[::1]" | "http://[::1]"[:/]*)
		PROTO='=http,https' ;;
	*) die "TC_RELEASE_BASE must be an https:// URL" ;;
	esac
}

# printable drops control bytes (escape sequences) from server-supplied text
# before it reaches the terminal.
printable() {
	printf '%s' "$1" | LC_ALL=C tr -cd '[:print:]'
}

latest_tag() {
	# /releases/latest redirects to /releases/tag/<tag>; no API, no rate limit.
	url=$(fetch -fsSLI -o /dev/null -w '%{url_effective}' "$1/latest") ||
		die "can't reach $1 (offline, or no releases yet?)"
	case "$url" in
	*/tag/*) ;;
	*) die "unexpected answer from $1/latest: $(printable "$url")" ;;
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

# verify_sig checks $1/checksums.txt against the release signature, or
# dies. Without it, checksums.txt only proves the download matches whatever
# the release host serves.
verify_sig() {
	if [ "${TC_INSECURE_NO_SIG:-}" = 1 ]; then
		say "WARNING: TC_INSECURE_NO_SIG=1, so the release signature is NOT checked."
		say "WARNING: anyone who can change the release can run code on this machine."
		return 0
	fi
	case "$ALLOWED_SIGNERS" in
	*REPLACE_WITH_MAINTAINER_KEY*)
		die "this install.sh has no release signing key, so it can't verify the download; use the install command from a tc release" ;;
	esac
	command -v ssh-keygen >/dev/null 2>&1 ||
		die "need ssh-keygen (OpenSSH 8.1 or newer) to verify the release signature"
	fetch -fsSL -o "$1/checksums.txt.sig" "$base/download/$tag/checksums.txt.sig" ||
		die "can't download checksums.txt.sig for $tag (an unsigned release?); not installing"
	printf '%s\n' "$ALLOWED_SIGNERS" >"$1/allowed_signers"
	ssh-keygen -Y verify -f "$1/allowed_signers" -I tc-release -n tc-release \
		-s "$1/checksums.txt.sig" <"$1/checksums.txt" >/dev/null 2>&1 ||
		die "checksums.txt for $tag is not signed by the tc release key; not installing"
	say "release signature OK"
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
	check_base "$base"
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
	fetch -fsSL -o "$tmp/checksums.txt" "$base/download/$tag/checksums.txt" ||
		die "can't download checksums.txt for $tag"
	verify_sig "$tmp"
	fetch -fsSL -o "$tmp/$asset" "$base/download/$tag/$asset" ||
		die "can't download $asset for $tag"

	want=$(awk -v f="$asset" '$2 == f || $2 == "*" f {print $1; exit}' "$tmp/checksums.txt")
	[ -n "$want" ] || die "checksums.txt has no entry for $asset"
	got=$(sha256_of "$tmp/$asset")
	if [ "$want" != "$got" ]; then
		die "checksum mismatch for $asset (expected $want, got $got); not installing"
	fi

	mkdir -p "$tmp/x"
	tar -xzf "$tmp/$asset" -C "$tmp/x" tc || die "can't unpack $asset"
	# -f follows symlinks; a symlink member must not pass as the binary.
	[ -f "$tmp/x/tc" ] && [ ! -L "$tmp/x/tc" ] || die "$asset has no tc binary"

	mkdir -p "$dir" || die "can't create $dir"
	# Copy next to the target, then rename: atomic, and a running tc keeps
	# working. mktemp makes a new, unpredictable 0600 file (never a symlink
	# someone planted in a shared dir), which cp then writes into.
	new=$(mktemp "$dir/.tc.new.XXXXXX") || die "can't write to $dir"
	cp "$tmp/x/tc" "$new" && chmod 755 "$new" && mv -f "$new" "$dir/tc" || {
		rm -f "$new"
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
			TC_INVITE=$invite "$dir/tc" join </dev/tty
		else
			TC_INVITE=$invite "$dir/tc" join </dev/null
		fi
	else
		say "next: $dir/tc join 'tc://…' with the invite your admin sent"
	fi
}

# Everything runs from here, so a partly downloaded script does nothing.
main "$@"
