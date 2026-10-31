import { Liino } from './dist/index.js';
import path from 'node:path';
import process from 'node:process';

const file = path.resolve(import.meta.dirname, '../testdata/tone-48000-stereo.ogg');
const liino = new Liino();

const cases = [
  { name: 'null/plain', cfg: { driver: 'null', volume: 0.2 }, opts: {} },
  { name: 'null/appname', cfg: { driver: 'null', volume: 0.2, app_name: 'Quaver Music' }, opts: {} },
  { name: 'null/autoplay', cfg: { driver: 'null', volume: 0.2 }, opts: { auto_play: true } },
  { name: 'pw/plain', cfg: { driver: 'pipewire', volume: 0.2 }, opts: {} },
  { name: 'pw/default', cfg: { volume: 0.2 }, opts: {} },
  { name: 'pw/autoplay', cfg: { driver: 'pipewire', volume: 0.2 }, opts: { auto_play: true } },
  { name: 'http-ish', cfg: { driver: 'null', volume: 0.2 }, opts: {}, uri: `file://${file}` },
  { name: 'mp3', cfg: { driver: 'null', volume: 0.2 }, opts: {}, uri: path.resolve(import.meta.dirname, '../testdata/tone-44100-stereo.mp3') },
  { name: 'flac51', cfg: { driver: 'pipewire', volume: 0.2 }, opts: {}, uri: path.resolve(import.meta.dirname, '../testdata/tone-48000-51.flac') },
];

for (const c of cases) {
  let p;
  try { p = liino.create(c.cfg); }
  catch (e) { console.log(`✗ ${c.name}: create threw ${String(e)}`); continue; }
  const events = [];
  try { p.open(c.uri ?? file, { auto_play: false, ...c.opts }); }
  catch (e) { console.log(`✗ ${c.name}: open threw ${String(e)}`); p.close(); continue; }
  for (let i = 0; i < 4; i++) {
    const raw = p.waitEvent(1200);
    if (!raw) break;
    const ev = JSON.parse(raw);
    events.push(`${ev.kind}${ev.snapshot?.error ? ` (${ev.snapshot.error})` : ''}`);
    if (['failed', 'ended', 'playing'].includes(ev.kind)) break;
  }
  const snap = p.snapshot() ? JSON.parse(p.snapshot()) : null;
  console.log(`${c.name}: ${events.join(' → ') || '(no event)'} state=${snap?.state} driver=${snap?.driver || '∅'} dec=${snap?.decoder || '∅'} dur=${snap?.duration_ms}`);
  p.close();
}
