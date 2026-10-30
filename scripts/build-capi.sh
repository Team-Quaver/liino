#!/usr/bin/env bash
# Build the Liino C ABI shared library that Node/Electron loads.
#
#	go run / scripts/build-capi.sh                 # host platform only
#	GOOS=linux GOARCH=arm64 scripts/build-capi.sh  # one target
#	scripts/build-capi.sh all                       # every target with a toolchain
#
# The audio pipeline is pure Go (CGO_ENABLED=0 everywhere it matters). Only this
# export layer uses cgo, because -buildmode=c-shared needs a C linker. That is
# why the package sits behind the `liino_capi` build tag.
#
# Cross builds need a C compiler for the target:
#	windows/amd64 -> x86_64-w64-mingw32-gcc (or set CC)
#	linux/arm64   -> aarch64-linux-gnu-gcc
#	darwin/*      -> the macOS toolchain (build on macOS, or osxcross)
# Targets without a usable compiler are reported and skipped, not faked.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

ext_for() { case "$1" in linux) echo so ;; darwin) echo dylib ;; windows) echo dll ;; *) echo bin ;; esac; }
prefix_for() { case "$1" in windows) echo "" ;; *) echo lib ;; esac; }

find_cc() { # find_cc <goos> <goarch>
	local goos=$1 goarch=$2
	if [ -n "${CC:-}" ]; then echo "$CC"; return; fi
	local v="${goos}_${goarch}"
	v="${v^^}"
	local ref="CC_$v"
	if [ -n "${!ref:-}" ]; then echo "${!ref}"; return; fi
	case "$goos/$goarch" in
	linux/amd64) command -v cc || true ;;
	linux/arm64) command -v aarch64-linux-gnu-gcc || true ;;
	linux/arm) command -v arm-linux-gnueabihf-gcc || true ;;
	linux/386) command -v cc || true ;;
	windows/amd64) command -v x86_64-w64-mingw32-gcc || true ;;
	windows/386) command -v i686-w64-mingw32-gcc || true ;;
	windows/arm64) command -v clang --target=aarch64-w64-windows-gnu 2>/dev/null || true ;;
	darwin/amd64) command -v o64-clang || command -v clang || true ;;
	darwin/arm64) command -v oa64-clang || command -v clang || true ;;
	esac
}

build_one() { # build_one <goos> <goarch>
	local goos=$1 goarch=$2 out dir cc
	local host_os host_arch
	host_os=$(go env GOOS)
	host_arch=$(go env GOARCH)
	out="build/${goos}-${goarch}"
	mkdir -p "$out"
	cc=$(find_cc "$goos" "$goarch")
	if [ -z "$cc" ]; then
		echo "SKIP ${goos}/${goarch}: no C compiler (set CC or CC_${goos^^}_${goarch^^})" >&2
		return 0
	fi
	local libname="$(prefix_for "$goos")liino.$(ext_for "$goos")"
	echo "BUILD ${goos}/${goarch} -> ${out}/${libname} (cc=$cc)"
	# CGO is required here and only here; -s -w keeps the library ~35% smaller.
	CGO_ENABLED=1 GOOS="$goos" GOARCH="$goarch" CC="$cc" \
		go build -trimpath -tags liino_capi -buildmode=c-shared \
		-ldflags "-s -w" -o "$out/$libname" ./capi
	cp "$out/libliino.h" "$out/liino.h" 2>/dev/null || cp capi/liino.h "$out/liino.h"
	rm -f "$out/libliino.h"
	[ "$goos/$goarch" = "$host_os/$host_arch" ] || echo "  (cross build: not smoke tested here)"
}

case "${1:-host}" in
all)
	build_one linux amd64
	build_one linux arm64
	build_one windows amd64
	build_one darwin arm64
	;;
host)
	build_one "$(go env GOOS)" "$(go env GOARCH)"
	;;
*)
	build_one "$1" "$2"
	;;
esac
