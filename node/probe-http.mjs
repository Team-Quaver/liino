// Liino + HTTP range streaming probe: does ffgo/avio playback work against a
// local /api/stream-style server (the shape Quaver's sidecar serves)?
// Events are drained with the NON-BLOCKING nextEvent() poll — waitEvent() would
// park this same JS thread and starve the HTTP server below it.
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { Liino } from './dist/index.js';

const file = path.resolve(import.meta.dirname, '../testdata/tone-48000-stereo.ogg');
const size = fs.statSync(file).size;

const server = http.createServer((req, res) => {
  const range = req.headers.range ? /bytes=(\d*)-(\d*)/.exec(req.headers.range) : null;
  console.log(`[http] ${req.method} ${req.url ?? ''} range=${req.headers.range ?? '-'}`);
  if (range) {
    const start = Number(range[1] ?? 0);
    const end = range[2] ? Number(range[2]) : Math.min(start + (1 << 20), size - 1);
    res.writeHead(206, {
      'Content-Range': `bytes ${start}-${end}/${size}`,
      'Accept-Ranges': 'bytes',
      'Content-Length': String(end - start + 1),
      'Content-Type': 'application/ogg',
    });
    fs.createReadStream(file, { start, end }).pipe(res);
    return;
  }
  res.writeHead(200, { 'Content-Length': String(size), 'Accept-Ranges': 'bytes', 'Content-Type': 'application/ogg' });
  fs.createReadStream(file).pipe(res);
});

await new Promise((r) => server.listen(0, '127.0.0.1', r));
const url = `http://127.0.0.1:${server.address().port}/api/stream/probe-token`;
console.log('[http] serving', url);

const liino = new Liino();
const driver = process.argv.find(a => a.startsWith('--driver='))?.slice(9) ?? 'null';
const p = liino.create({ driver, volume: 0.15, app_name: 'Quaver Music' });
p.open(url, { auto_play: true });

const done = await new Promise((resolve) => {
  const started = Date.now();
  const timer = setInterval(() => {
    for (let i = 0; i < 8; i++) {
      const raw = p.nextEvent();
      if (!raw) break;
      const ev = JSON.parse(raw);
      const s = ev.snapshot ?? {};
      console.log(`[ev] ${ev.kind} state=${s.state} pos=${s.position_ms}/${s.duration_ms} buf=${s.buffered_ms} codec=${s.codec} live=${s.live}${s.error ? ` err=${s.error}` : ''}`);
      if (ev.kind === 'failed' || ev.kind === 'ended') { clearInterval(timer); resolve(ev.kind); return; }
    }
    const c = JSON.parse(p.clock() ?? '{}');
    if (c.position_ms) console.log(`[clock] pos=${Math.round(c.position_ms)} buffer=${Math.round(c.buffer_pos_ms)} latency=${Math.round(c.latency_ms)}`);
    if (Date.now() - started > 8000) { clearInterval(timer); resolve('timeout'); }
  }, 100);
});

console.log('[probe] finished:', done);
p.close();
server.close();
