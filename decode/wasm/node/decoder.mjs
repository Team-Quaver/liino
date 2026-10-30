#!/usr/bin/env node
// Liino libav.js decode helper.
//
// Fallback path only: Liino's primary decoder is native FFmpeg through ffgo.
// This helper exists for builds that must not depend on system FFmpeg
// libraries. It is a plain Node process driven by NDJSON on stdin and answers
// with framed binary on stdout:
//
//	[uint32 BE payload length][1 byte kind][payload]
//	kind 'J' = JSON event, 'P' = little-endian float32 interleaved PCM
//
// Commands:
//	{"cmd":"open","uri":"file|http|path","format":"mp3"}
//	{"cmd":"seek","seconds":12.5}
//	{"cmd":"close"}
//
// Requires the `libav.js` package (npm i libav.js). Needs a host-side smoke
// run: it is not exercised in CI, since CI builds the pure-Go library only.
import { readFileSync } from 'node:fs';
import process from 'node:process';

const OUT_RATE = 48000;
const OUT_CHANNELS = 2;

let LibAV;
try {
  ({ default: LibAV } = await import('libav.js'));
} catch (err) {
  emit({ event: 'error', error: `libav.js not installed: ${err.message}` });
  process.exit(2);
}

let current = null;

function emit(obj) {
  const body = Buffer.from(JSON.stringify(obj), 'utf8');
  const head = Buffer.alloc(5);
  head.writeUInt32BE(body.length, 0);
  head.writeUInt8(0x4a /* J */, 4);
  process.stdout.write(head);
  process.stdout.write(body);
}

function emitPCM(f32) {
  const body = Buffer.from(f32.buffer, f32.byteOffset, f32.byteLength);
  const head = Buffer.alloc(5);
  head.writeUInt32BE(body.length, 0);
  head.writeUInt8(0x50 /* P */, 4);
  process.stdout.write(head);
  process.stdout.write(body);
}

async function openSource(uri, format) {
  const libav = await LibAV.LibAV({ yuvbuf: 1 });
  const args = ['-hide_banner', '-nostats', '-threads', '1'];
  if (format) args.push('-f', format);
  args.push('-i', await stage(libav, uri),
    '-vn', '-sn', '-dn',
    '-acodec', 'pcm_f32le', '-ac', String(OUT_CHANNELS), '-ar', String(OUT_RATE),
    '-f', 'f32le', '-');

  // stdout of the WASM ffmpeg run is pumped through an fd interface into our
  // framed writer, so the Go side sees a stream, not one giant buffer.
  let pending = Buffer.alloc(0);
  const fd_iface = {
    seek: () => 0,
    read: () => 0,
    write: (fd, buf_addr, buf_ptr, size) => {
      const chunk = new Uint8Array(libav.HEAPU8.buffer, buf_ptr, size);
      pending = Buffer.concat([pending, Buffer.from(chunk)]);
      const frameBytes = 1024 * OUT_CHANNELS * 4;
      while (pending.length >= frameBytes) {
        emitPCM(new Float32Array(pending.subarray(0, frameBytes).buffer));
        pending = pending.subarray(frameBytes);
      }
      return size;
    },
    close: () => 0,
  };
  await libav.mkstdout(fd_iface);
  const res = await libav.ffmpeg(args);
  await libav.deleteopt('stdout');
  if (pending.length > 0) {
    emitPCM(new Float32Array(pending.buffer, pending.byteOffset, (pending.length >> 2) << 2));
  }
  emit({ event: 'eof', rc: res });
  return libav;
}

async function stage(libav, uri) {
  // file paths and http(s) URLs both become an in-WASM input file.
  if (/^https?:/i.test(uri)) {
    const res = await fetch(uri);
    if (!res.ok) throw new Error(`fetch ${res.status}`);
    const buf = new Uint8Array(await res.arrayBuffer());
    const name = '/input.bin';
    await libav.fs().writeFile(name, buf);
    return name;
  }
  const path = uri.replace(/^file:\/\//, '');
  const buf = new Uint8Array(readFileSync(path));
  await libav.fs().writeFile('/input.bin', buf);
  return '/input.bin';
}

const rl = (await import('node:readline')).createInterface({ input: process.stdin });
for await (const line of rl) {
  let cmd;
  try {
    cmd = JSON.parse(line);
  } catch {
    continue;
  }
  try {
    if (cmd.cmd === 'open') {
      emit({ event: 'meta', sample_rate: OUT_RATE, channels: OUT_CHANNELS, duration: 0, codec: 'libav.js', seekable: false });
      current = openSource(cmd.uri, cmd.format);
      current.catch((err) => emit({ event: 'error', error: String(err && err.message ? err.message : err) }));
    } else if (cmd.cmd === 'seek') {
      emit({ event: 'error', error: 'seek needs range support in the helper' });
    } else if (cmd.cmd === 'close') {
      current = null;
      break;
    }
  } catch (err) {
    emit({ event: 'error', error: String(err) });
  }
}
