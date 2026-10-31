# Liino — Quaver 原生音频管线（Pure Go）

Liino 是 Quaver Music / Quaver Music Astra 的下一代音频后端，用纯 Go 实现
（`CGO_ENABLED=0`），与既有 mpv/ffmpeg 子进程方案互不干扰。

## 定位

- **解码**：两套后端可切换。
  - `Native`（Quaver Music Astra）：通过 [ffgo](https://github.com/obinnaokechukwu/ffgo)
    动态加载系统 FFmpeg（libav\*），无 CGO、无捆绑二进制。
  - `WASM`（Quaver Music）：通过 [libav.js](https://github.com/Yahweasel/libav.js)
    在 Node 侧解码，Go 侧消费 float32 PCM。
- **PCM 管线**：统一转成 48 kHz / float32 交织，保留源声道数（5.1、7.1、7.1.4…）；
  输出端能承载多声道时原样送出（PipeWire 由 graph 决定是否下混），否则在播放器里
  折成双声道。环形缓冲解耦解码与设备回调。
- **DSP**：增益 → 效果器链（双二阶 EQ、限幅器等），纯 Go 实现。
- **输出**：`Output` 接口 + 平台注册表。
  - Linux：PipeWire（[purego-pipewire](https://github.com/bnema/purego-pipewire)，`CGO_ENABLED=0`），
    不用 ALSA。
  - Windows：WASAPI 共享模式（[audio-go](https://github.com/vcaesar/audio-go)），
    随进程走，不破坏系统定向音频捕获（loopback）。

## 目录

```
liino.go        // 门面：Open / Load / Player
core.go         // 播放引擎（解码线程 + 泵 + 输出）
decode/         // DecodeBackend 接口 + native / wasm 实现
pcm/            // 采样转换、环形缓冲
dsp/            // 增益、EQ、限幅器、效果链
output/         // Output 接口 + 平台后端
cmd/liino-play  // 示例播放器
```

## 构建

```sh
cd vendor/Liino
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...
```

Native 解码在运行期需要系统提供 FFmpeg 共享库（Linux：`libavformat.so` 等，
Windows：`avcodec-*.dll` 等）。WASM 模式需要 Node.js 与 libav.js 资产，见
`decode/wasm/`。

## 许可

AGPL-3.0（见 `LICENSE`）。
