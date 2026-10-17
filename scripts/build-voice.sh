#!/bin/sh
# build-voice.sh OS ARCH VERSION OUTDIR: builds the voice flavor of tc
# (-tags voice, cgo) with libopus linked statically, and packs it as
# OUTDIR/tc-voice_<os>_<arch>.tar.gz (binary named tc, like the default
# build, so install.sh and tc update treat both alike).
#
# libopus comes from a pinned, checksummed xiph.org release built here, so
# every target links the same codec and no target needs libopus at runtime.
# Runtime dependencies left: macOS — system frameworks (CoreAudio,
# AudioToolbox); Linux — glibc (libc, libm, libdl, libpthread) of the build
# machine or newer; the audio stack (PulseAudio/PipeWire/ALSA) is dlopen'd
# by miniaudio when a huddle starts.
#
# Builds for the host OS; on macOS it also cross-builds the other arch
# (clang -arch). Linux arm64 needs an arm64 runner (no cross toolchain).
# Used by .github/workflows/release.yml and ci.yml.
set -eu

[ "$#" -eq 4 ] || { echo "usage: build-voice.sh OS ARCH VERSION OUTDIR" >&2; exit 2; }
os=$1 arch=$2 version=$3 out=$4

OPUS_VERSION=1.5.2
OPUS_SHA256=65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1

root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=${TMPDIR:-/tmp}/tc-voice-build.$$
mkdir -p "$work"
trap 'rm -rf "$work"' EXIT

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi
}

case "$os/$arch" in
darwin/arm64) carch=arm64 host=aarch64-apple-darwin ;;
darwin/amd64) carch=x86_64 host=x86_64-apple-darwin ;;
linux/amd64 | linux/arm64) carch="" host="" ;;
*) echo "build-voice.sh: unsupported target $os/$arch" >&2; exit 2 ;;
esac
hostos=$(uname -s | tr '[:upper:]' '[:lower:]')
[ "$hostos" = "$os" ] || { echo "build-voice.sh: build $os on $os (host is $hostos)" >&2; exit 2; }

cc=cc
cflags="-O2 -fPIC"
if [ "$os" = darwin ]; then
	# Same floor as Go 1.25's own macOS support.
	export MACOSX_DEPLOYMENT_TARGET=${MACOSX_DEPLOYMENT_TARGET:-11.0}
	cc="clang -arch $carch"
fi

tarball=$work/opus-$OPUS_VERSION.tar.gz
curl -q -fsSL --proto '=https' --tlsv1.2 -o "$tarball" "https://downloads.xiph.org/releases/opus/opus-$OPUS_VERSION.tar.gz"
got=$(sha256 "$tarball")
[ "$got" = "$OPUS_SHA256" ] || { echo "build-voice.sh: opus tarball sha256 $got, want $OPUS_SHA256" >&2; exit 1; }
tar -C "$work" -xzf "$tarball"

prefix=$work/opus-prefix
(
	cd "$work/opus-$OPUS_VERSION"
	set -- --prefix="$prefix" --disable-shared --enable-static --disable-doc --disable-extra-programs
	[ -z "$host" ] || set -- "$@" --host="$host"
	CC=$cc CFLAGS=$cflags ./configure "$@" >"$work/configure.log" 2>&1 || { tail -50 "$work/configure.log" >&2; exit 1; }
	make -j4 >"$work/make.log" 2>&1 || { tail -50 "$work/make.log" >&2; exit 1; }
	make install >/dev/null
)

name=tc-voice_${os}_${arch}
bin=$work/$name/tc
mkdir -p "$work/$name"
# Only our static libopus is visible to pkg-config (no .so/.dylib in it),
# so the linker can't pick a system libopus.
(
	cd "$root"
	PKG_CONFIG_PATH=$prefix/lib/pkgconfig PKG_CONFIG_LIBDIR=$prefix/lib/pkgconfig \
		CGO_ENABLED=1 GOOS=$os GOARCH=$arch CC=$cc GOTOOLCHAIN=local \
		go build -tags voice -trimpath -ldflags "-s -w -X main.version=$version" -o "$bin" ./cmd/tc
)

# No dynamic libopus, whatever the build machine had installed.
if [ "$os" = darwin ]; then
	otool -L "$bin" >"$work/deps"
else
	ldd "$bin" >"$work/deps" 2>&1 || true
fi
if grep -i opus "$work/deps" >/dev/null; then
	echo "build-voice.sh: $bin links libopus dynamically:" >&2
	cat "$work/deps" >&2
	exit 1
fi
cat "$work/deps"

{
	echo "tc $version (voice build) for $os/$arch — terminal team chat with voice huddles"
	echo
	echo "Put tc on your PATH (e.g. ~/.local/bin), then join with the invite you were sent:"
	echo "  tc join 'tc://host:7443/…'"
	echo
	echo "In chat: /huddle joins the channel's voice huddle; hold ctrl+t to talk."
	if [ "$os" = linux ]; then
		echo "Needs glibc 2.35 or newer and PulseAudio, PipeWire (pulse) or ALSA."
	fi
	echo
	echo "https://github.com/pranavjayakumar/terminal-chat"
} >"$work/$name/README.txt"
tar -C "$work/$name" -czf "$out/$name.tar.gz" tc README.txt
ls -l "$out/$name.tar.gz"
