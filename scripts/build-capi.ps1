#!/usr/bin/env pwsh
# Build the Liino C ABI shared library that Node/Electron loads (PowerShell).
#
#	.\scripts\build-capi.ps1                  # host platform only
#	.\scripts\build-capi.ps1 all              # every target with a toolchain
#	.\scripts\build-capi.ps1 linux arm64      # one specific target
#	.\scripts\build-capi.ps1 -Target windows -Arch amd64
#
# The audio pipeline is pure Go (CGO_ENABLED=0 everywhere it matters). Only this
# export layer uses cgo, because -buildmode=c-shared needs a C linker. That is
# why the package sits behind the `liino_capi` build tag.
#
# Cross builds need a C compiler for the target:
#	windows/amd64 -> x86_64-w64-mingw32-gcc (or set CC)
#	linux/arm64   -> aarch64-linux-gnu-gcc
#	darwin/*      -> the macOS toolchain (build on macOS, or osxcross)
# On Windows itself, `gcc` (MSYS2/mingw-w64) or `clang` builds windows/* natively;
# a bare compiler is only accepted when the target OS matches the host OS, so a
# cross target never gets silently compiled into the host format.
# Targets without a usable compiler are reported and skipped, not faked.
#
# This mirrors scripts/build-capi.sh argument for argument. Run it with
# PowerShell 7 (`pwsh`) or Windows PowerShell 5.1.

[CmdletBinding()]
param(
	[Parameter(Position = 0)]
	[string]$Target = "host",

	[Parameter(Position = 1)]
	[string]$Arch = ""
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $root

# The machine's real OS, independent of any GOOS the caller exported. Used only
# to decide whether a bare gcc/clang actually targets the requested OS.
# Windows PowerShell 5.1 hits the first branch; `$IsMacOS` is never evaluated
# there, so StrictMode does not complain about the 5.1-only undefined variable.
$script:HostOs =
	if ($PSVersionTable.PSEdition -eq "Desktop") { "windows" }
	elseif ($env:OS -eq "Windows_NT") { "windows" }
	elseif ($IsMacOS) { "darwin" }
	else { "linux" }

function Get-GoEnv([string]$name) {
	$value = & go env $name
	if ($LASTEXITCODE -ne 0) {
		throw "go env $name failed (exit $LASTEXITCODE)"
	}
	return $value.Trim()
}

function Get-ExtFor([string]$goos) {
	switch ($goos) {
		"linux" { return "so" }
		"darwin" { return "dylib" }
		"windows" { return "dll" }
		default { return "bin" }
	}
}

function Get-PrefixFor([string]$goos) {
	if ($goos -eq "windows") { return "" }
	return "lib"
}

# Find-Command returns the first executable from the candidate list, or $null.
# Keep the order: on Windows a mingw-w64 cross gcc must win over a bare `gcc`
# that may belong to another toolchain.
function Find-Command([string[]]$names) {
	foreach ($name in $names) {
		$cmd = Get-Command $name -CommandType Application -ErrorAction SilentlyContinue |
			Select-Object -First 1
		if ($cmd) { return $cmd.Source }
	}
	return $null
}

function Find-CC([string]$goos, [string]$goarch) {
	if ($env:CC) { return $env:CC }

	$ref = "CC_$($goos.ToUpper())_$($goarch.ToUpper())"
	$value = [Environment]::GetEnvironmentVariable($ref)
	if ($value) { return $value }

	# A bare gcc/clang targets whatever the host is. Only accept it when the
	# target OS is the host OS; cross builds need the prefixed toolchain,
	# otherwise `all` on Linux would hand windows/amd64 to the Linux gcc and
	# fail inside runtime/cgo instead of skipping cleanly.
	$native = $script:HostOs -eq $goos

	switch ("$goos/$goarch") {
		"linux/amd64" {
			if ($native) { return Find-Command @("cc", "gcc") }
			return Find-Command @("x86_64-linux-gnu-gcc")
		}
		"linux/386" {
			if ($native) { return Find-Command @("cc", "gcc") }
			return Find-Command @("i686-linux-gnu-gcc")
		}
		"linux/arm64" { return Find-Command @("aarch64-linux-gnu-gcc") }
		"linux/arm" { return Find-Command @("arm-linux-gnueabihf-gcc") }
		# Native Windows builds normally use gcc from MSYS2/mingw-w64; the
		# x86_64-w64-mingw32-gcc name is the cross-toolchain spelling used on
		# Linux. clang only counts on Windows, where it targets MSVC/mingw ABIs.
		"windows/amd64" {
			if ($native) { return Find-Command @("gcc", "clang", "cc") }
			return Find-Command @("x86_64-w64-mingw32-gcc")
		}
		"windows/386" {
			if ($native) { return Find-Command @("gcc", "clang", "cc") }
			return Find-Command @("i686-w64-mingw32-gcc")
		}
		"windows/arm64" {
			if ($native) { return Find-Command @("aarch64-w64-mingw32-gcc", "clang") }
			return Find-Command @("aarch64-w64-mingw32-gcc")
		}
		"darwin/amd64" {
			if ($native) { return Find-Command @("clang") }
			return Find-Command @("o64-clang")
		}
		"darwin/arm64" {
			if ($native) { return Find-Command @("clang") }
			return Find-Command @("oa64-clang")
		}
		default { return $null }
	}
}

# Invoke-GoBuild runs the c-shared build with the target environment applied
# only for this call, then restores every variable it touched so the caller's
# shell (and any later target in `all`) starts clean.
function Invoke-GoBuild([string]$goos, [string]$goarch, [string]$cc, [string]$outFile) {
	$names = @("CGO_ENABLED", "GOOS", "GOARCH", "CC")
	$saved = @{}
	foreach ($name in $names) {
		$saved[$name] = [Environment]::GetEnvironmentVariable($name)
	}

	try {
		$env:CGO_ENABLED = "1"
		$env:GOOS = $goos
		$env:GOARCH = $goarch
		$env:CC = $cc
		# CGO is required here and only here; -s -w keeps the library ~35% smaller.
		& go build -trimpath -tags liino_capi -buildmode=c-shared `
			-ldflags "-s -w" -o $outFile ./capi
		if ($LASTEXITCODE -ne 0) {
			throw "go build failed for $goos/$goarch (exit $LASTEXITCODE)"
		}
	}
	finally {
		foreach ($name in $names) {
			if ($null -eq $saved[$name]) {
				Remove-Item "Env:$name" -ErrorAction SilentlyContinue
			}
			else {
				Set-Item "Env:$name" $saved[$name]
			}
		}
	}
}

function Build-One([string]$goos, [string]$goarch) {
	$hostOs = Get-GoEnv "GOOS"
	$hostArch = Get-GoEnv "GOARCH"

	$out = Join-Path "build" "$goos-$goarch"
	New-Item -ItemType Directory -Force -Path $out | Out-Null

	$cc = Find-CC $goos $goarch
	if (-not $cc) {
		$upper = "$($goos.ToUpper())_$($goarch.ToUpper())"
		Write-Warning "SKIP $goos/${goarch}: no C compiler (set CC or CC_$upper)"
		return
	}

	$libname = "$(Get-PrefixFor $goos)liino.$(Get-ExtFor $goos)"
	Write-Output "BUILD $goos/$goarch -> $out/$libname (cc=$cc)"

	$outFile = Join-Path $out $libname
	Invoke-GoBuild $goos $goarch $cc $outFile

	# Go names the generated header after the output base: libliino.so gets
	# libliino.h, liino.dll gets liino.h. Ship the stable liino.h name; when
	# there is no libliino.h (the DLL name already matches), fall back to the
	# checked-in C API header.
	$generatedHeader = Join-Path $out "libliino.h"
	$header = Join-Path $out "liino.h"
	if (Test-Path $generatedHeader) {
		Copy-Item $generatedHeader $header -Force
	}
	else {
		Copy-Item (Join-Path (Join-Path $root "capi") "liino.h") $header -Force
	}
	Remove-Item $generatedHeader -ErrorAction SilentlyContinue

	if ("$goos/$goarch" -ne "$hostOs/$hostArch") {
		Write-Output "  (cross build: not smoke tested here)"
	}
}

if (-not (Get-Command go -CommandType Application -ErrorAction SilentlyContinue)) {
	throw "go is not on PATH"
}

switch ($Target) {
	"all" {
		Build-One "linux" "amd64"
		Build-One "linux" "arm64"
		Build-One "windows" "amd64"
		Build-One "darwin" "arm64"
	}
	"host" {
		Build-One (Get-GoEnv "GOOS") (Get-GoEnv "GOARCH")
	}
	default {
		if (-not $Arch) {
			throw "usage: build-capi.ps1 [host|all|<goos> <goarch>]"
		}
		Build-One $Target $Arch
	}
}
