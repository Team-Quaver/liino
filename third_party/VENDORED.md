# Vendored third-party code

## ffgo

- Upstream: <https://github.com/obinnaokechukwu/ffgo> (tag `v0.1.1`, module path
  later renamed to `github.com/nonibytes/ffgo`, which no longer resolves on the
  proxy — the repository and the declared module path disagree, so the version
  cannot be required normally).
- License: Apache-2.0. The upstream `LICENSE` is kept verbatim in `ffgo/`.
- Copyright: upstream authors. This is a *fork*, not a relicense: Liino is
  AGPL-3.0-only, the fork stays Apache-2.0, and the two are combined inside the
  AGPL work as permitted by Apache-2.0 §4.
- Import path: `github.com/team-quaver/ffgo`, wired through a `replace` in
  `go.mod`. Any consumer that pulls Liino as a module dependency must add the
  same replace, or point at a published Team-Quaver fork.

### Modifications

1. `internal/dl` (new): single loader entry point `Open`/`Close`.
   - `dl_unix.go` keeps upstream behaviour (`purego.Dlopen` with
     `RTLD_NOW|RTLD_GLOBAL`).
   - `dl_windows.go` (new) loads DLLs with `LoadLibraryExW` plus
     `LOAD_WITH_ALTERED_SEARCH_PATH`, so an app-local FFmpeg build resolves its
     sibling DLLs without touching `PATH`.
2. `internal/bindings/bindings.go`: `tryOpen` now calls `dl.Open`.
3. `internal/shim/shim.go`: the optional C shim loads through `dl.Open`.
4. Module path rewritten from `github.com/nonibytes/ffgo` to
   `github.com/team-quaver/ffgo` across all sources.
5. Test data, examples, docs and CI metadata from the release archive are not
   copied; only the packages Liino compiles are kept.
6. `internal/layout` (new): version-aware field offsets for `AVFrame`,
   `AVCodecParameters`, `AVFormatContext` and `AVStream`.
   - Upstream hardcoded one set of offsets "verified on FFmpeg 6.x" and several
     of them were wrong even there (`AVFrame.sample_rate` was read at 216, the
     real 6.x offset is 208; `AVCodecParameters.channels` was read at 148, which
     is not an `AVChannelLayout` offset in any release). On FFmpeg 8 every one of
     those reads lands on a different field, so decoded frames reported
     `rate = 0`, `channels = 0` and `duration = 0`.
   - The tables were measured by compiling `offsetof()` programs against the
     public headers of FFmpeg 5.1.8, 6.1.3, 7.1.1 and 8.1.3 and selecting them at
     runtime from `avutil_version()`. Regenerating them for a new release:
     `gcc -I <ffmpeg-src> off.c && ./a.out` against
     `libavutil/frame.h`, `libavutil/channel_layout.h`,
     `libavcodec/codec_par.h`, `libavformat/avformat.h`, `libavcodec/avcodec.h`.
   - Channel counts additionally fall back to buffer geometry (plane pointers,
     `linesize`, `nb_samples`, `av_get_bytes_per_sample`/
     `av_get_sample_fmt_name`), so a release newer than the last measured table
     degrades instead of feeding garbage to the resampler.
   - `avutil.GetFrameSampleRate`, `GetFrameChannels`, `SetFrameSampleRate`,
     `FrameSetChannels`, `GetFrameKeyFrame` and the `avformat.GetCodecPar*` /
     `GetStream*` / `AVFormatContext` readers now go through `internal/layout`.
   - Not converted: `avcodec`'s `AVCodecContext` field offsets (encoder and
     hardware paths only — Liino decodes, never encodes). They are still measured
     for FFmpeg 6.x and are wrong on 7.x/8.x.
   - The optional C shim (`shim/`, prebuilt) is no longer trusted ahead of the
     tables: it bakes `offsetof()` values in at *its* build time, which says
     nothing about the FFmpeg loaded at *run* time. It stays as a fallback for
     releases without a measured table.
   - AGPL note: this fork stays Apache-2.0, see the license section above.
7. `frame.go`: new `FrameWrapper.SetChannelLayout(channels)` writes the default
   `AVChannelLayout` for a channel count onto a decoder-owned frame. Opus
   mapping family 3 (QQ Music's 7.1.4 tier) decodes 12 planes but leaves
   `AVFrame.ch_layout` unspecified; `swr_convert_frame` then fails on the first
   frame. Liino calls it before resampling so the data can be copied
   channel-faithfully.

### Why

Upstream ffgo cannot be built for Windows/AMD64 at all: `purego.Dlopen` has no
Windows implementation, so `internal/bindings` fails to compile
(`undefined: purego.Dlopen`). Symbol resolution, by contrast, already works on
Windows because `purego.loadSymbol` maps to `syscall.GetProcAddress`. A module
handle from `LoadLibraryExW` is therefore the only missing piece.

### Removing the fork

Drop this directory and the `replace` once upstream (or a published Team-Quaver
fork) ships a Windows-capable loader, then bump `decode/native` back to the
upstream import path.

## purego-pipewire

- Upstream: <https://github.com/bnema/purego-pipewire> (tag `v0.1.6`).
- License: MIT. The upstream `LICENCE` is kept verbatim.
- Import path: same module path, wired through a `replace` in `go.mod`; only the
  packages Liino compiles are copied.

### Modifications

1. `PlayerConfig` (public and `internal/core`) gained `AppName`, `StreamName`
   and `MediaRole`.
2. `StreamOps.CreatePlaybackStream` takes a `StreamProperties` value.
3. `internal/capi` builds a `spa_dict` and calls `pw_properties_new_dict`, then
   passes it to `pw_stream_new_simple` instead of `nil`. This is what makes the
   OS mixer show **Quaver Music** instead of the host process name
   (`electron`); the daemon otherwise derives `application.name` from
   `pw_get_prgname()` (a Node `process.title` change does not reach it).

### Why

Upstream's player API has no way to set stream properties, and the stream's
node/client therefore kept the default name. There is no runtime workaround
without the stream pointer (which upstream does not expose).

### Removing the fork

Drop this directory and the `replace` once upstream (or a published Team-Quaver
fork) exposes stream properties, then construct `pw.PlayerConfig` directly.
