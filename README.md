# Liino — Quaver 原生音频管线（Pure Go）

Liino 是 Quaver Music / Quaver Music Astra 的下一代音频后端，用纯 Go 实现
（`CGO_ENABLED=0`），与既有 mpv/ffmpeg 子进程方案互不干扰。

## 定位

- **解码**：按平台优先、按源回退。
  - `wmf`（Windows）：Media Foundation Source Reader
    （[go-bindings-win32](https://github.com/deploymenttheory/go-bindings-win32)）。
    进程内走系统 MFT 解码/重采样，MP3/AAC/FLAC/ALAC/WMA/Core Audio 等能力取决于
    机器已安装的 codec pack，无需捆绑 FFmpeg。
  - `avf`（macOS）：AVFoundation + CoreMedia
    （[go-bindings-macosplatform](https://github.com/deploymenttheory/go-bindings-macosplatform)）。
    AVAssetReader 使用与 QuickTime/Finder 相同的 Core Audio 解码器。
  - `native`：通过 [ffgo](https://github.com/obinnaokechukwu/ffgo) 动态加载系统
    FFmpeg（libav\*），无 CGO、无捆绑二进制。Linux 上的主后端，Windows/macOS 上的
    回退：OS 解码器覆盖不了的 Ogg/Opus、APE、DSD、7.1.4 Opus 等交给它。
  - `wasm`：通过 [libav.js](https://github.com/Yahweasel/libav.js) 在 Node 侧解码，
    最后兜底。
  - `decode.OpenAuto` 先按优先级尝试平台后端；`Guard.Supports` 提前排除明显不支持的
    容器，`ErrUnsupported` 触发下一个后端。`Config.Decoder` 留空即启用这条回退链，
    显式指定后端则只使用该后端。
- **能力侦测**：`decode.Statuses()` / `liino_diagnostics` 的每个后端带
  `codecs` 字段。Linux 上通过 `avcodec_find_decoder_by_name` 实测已安装 FFmpeg
  实际编入的解码器（mp3/aac/flac/opus/vorbis/dts/truehd/…），
  发行版精简 libavcodec 时不会静默失败。
- **PCM 管线**：统一转成 48 kHz / float32 交织，保留源声道数（5.1、7.1、7.1.4…）；
  输出端能承载多声道时原样送出（PipeWire 由 graph 决定是否下混），否则在播放器里
  折成双声道。环形缓冲解耦解码与设备回调。
- **DSP**：增益 → 效果器链（双二阶 EQ、限幅器等），纯 Go 实现。
- **输出**：`Output` 接口 + 平台注册表。
  - Linux：PipeWire（[purego-pipewire](https://github.com/bnema/purego-pipewire)，`CGO_ENABLED=0`），
    不用 ALSA。
  - Windows：WASAPI 共享模式（[audio-go](https://github.com/vcaesar/audio-go)），
    随进程走，不破坏系统定向音频捕获（loopback）。
  - macOS：Core Audio（同一 audio-go 后端）。

## 目录

```
liino.go        // 门面：Open / Load / Player
pipeline.go     // 播放引擎（解码线程 + 泵 + 输出）
decode/         // Backend 接口 + 注册表 + 按源回退
decode/wmf/     // Windows Media Foundation（Source Reader）
decode/avf/     // macOS AVFoundation + CoreMedia
decode/native/  // ffgo（Linux 主后端 / 其他平台回退）
decode/wasm/    // libav.js 兜底
pcm/            // 采样转换、环形缓冲
dsp/            // 增益、EQ、限幅器、效果链
output/         // Output 接口 + 平台后端
```

## 构建

```sh
cd vendor/Liino
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...
```

平台解码器随 OS 提供：Windows 需要 Media Foundation（N 版系统装 Media Feature
Pack），macOS 使用系统 AVFoundation。`native` 回退在运行期需要系统 FFmpeg 共享库
（Linux：`libavformat.so` 等，Windows：`avcodec-*.dll` 等）。WASM 模式需要 Node.js
与 libav.js 资产，见 `decode/wasm/`。

三个平台都能无 CGO 交叉编译：

```sh
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build ./...
```

查看当前机器实际可用的解码能力：宿主调用 `liino_diagnostics`，
`decoders[].codecs` 即实测结果；Go 里对应 `liino.Diagnose().Decoders`。

### 平台回退边界

- WMF/AVFoundation 都只接受 URL（file/http/https），`decode.Source.Reader`
  字节流会被拒绝并交给 ffgo。带自定义 `User-Agent`/Cookie 的云盘直链同理：
  OS 解码器拿不到这些请求头时由 ffgo 处理。
- Windows 需要 Media Foundation（Windows N 版要装 Media Feature Pack）；
  macOS 的 AVFoundation 访问本地 sidecar `http://127.0.0.1` 时，打包的
  `Info.plist` 需放行本地网络（`NSAppTransportSecurity` /
  `NSAllowsLocalNetworking`）。
- 平台解码器不支持的容器（Ogg/Opus、APE、WavPack、DSD、WMA 在 macOS 上等）
  由 `ErrUnsupported` 触发回退；此时只有装了系统 FFmpeg 才会成功。

## 依赖许可

- [go-bindings-win32](https://github.com/deploymenttheory/go-bindings-win32)：MIT
- [go-bindings-macosplatform](https://github.com/deploymenttheory/go-bindings-macosplatform)：MIT
- ffgo / purego-pipewire：见 `third_party/VENDORED.md`

## 许可

AGPL-3.0（见 `LICENSE`）。
