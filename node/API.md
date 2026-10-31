# @team-quaver/liino-node

Electron/Node FFI binding for `libliino`. Audio stays native: WASAPI on Windows
(same-process render session, per-app capture works), Core Audio on macOS,
PipeWire on Linux. Node does not resample or decode.

```js
import { Liino } from '@team-quaver/liino-node';

const liino = new Liino(); // libraryPath/defaultLibrary() resolves build/<platform>-<arch>/

console.log(liino.version);

const player = liino.create({
  driver: '', // default platform driver; use "null" for headless CI
  volume: 0.75,
  eq: [{ freq: 60, q: 0.7, gain: 2 }],
  limiter: true,
});

player.open('file:///music/song.flac', { auto_play: true });
player.waitEvent(5000); // { kind: 'loaded', snapshot: ... }

// Raise the volume without restarting playback.
player.setVolume(0.4);

// Position is media time; seek is asynchronous and updates seek_gen.
player.seek(30_000);

// For sub-40ms precision (lyrics, spectrum, A/V sync), query the clock
// directly instead of waiting for the next event.
const clock = player.clock();
console.log('audible position:', clock.position_ms, 'latency:', clock.latency_ms);

player.stop();
player.close();
```

## Shapes

- `create(config)` config keys: `decoder`, `driver`, `device_id`, `app_name`,
  `stream_name`, `ring_seconds`, `preroll_ms`, `period_ms`, `fade_out_ms`,
  `volume`, `limiter`, `limiter_db`, `eq`.
- `snapshot()` returns `{ state, position_ms, duration_ms, buffered_ms,
  volume, muted, driver, device, decoder, codec, sample_rate, channels,
  underruns, clips, peak_l_db, peak_r_db, live }`. `channels` is the source
  layout (a 7.1.4 Opus reports 12) even when the endpoint only accepts stereo.
- `clock()` returns a point-in-time high-precision reading:
  `{ position_ms, buffer_pos_ms, latency_ms, latency_ns, monotonic_ns,
  frames, sample_rate }`. `position_ms` subtracts the output latency from
  `buffer_pos_ms` to estimate what the listener hears; `monotonic_ns` is
  Go's monotonic clock for aligning with Electron's `performance.now()`.
- Event kinds: `loaded`, `playing`, `paused`, `buffering`, `ready`, `ended`,
  `stopped`, `failed`, `closed`. Every event carries monotonic `gen` and
  `seek_gen`; drop callbacks for older generations.
- `level()` returns peak/RMS meters and frame counts.

Strings returned through the C ABI are copied and freed immediately by the
binding; you do not need to call `liino_free_string` from JS.

## Build / deployment

Build one native library per platform and ship it next to your app:

```sh
cd vendor/Liino
scripts/build-capi.sh all
```

`defaultLibrary()` searches `vendor/Liino/build/<platform>-<arch>` first, then
`vendor/Liino/node/lib/<platform>-<arch>`. In an Electron app, point `new
Liino('/path/to/libliino.so')` at wherever the library is packaged.
