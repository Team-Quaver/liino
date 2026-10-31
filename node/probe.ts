#!/usr/bin/env node
// Ad-hoc probe: dump Liino diagnostics, devices, and the raw event stream.
import { Liino } from './index.js';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const args = process.argv.slice(2);
const driver = args.find(a => a.startsWith('--driver='))?.slice(9) ?? '';
const file = args.find(a => a.startsWith('--file='))?.slice(7)
  ?? path.resolve(import.meta.dirname ?? '.', '../../testdata/tone-48000-stereo.ogg');
const uri = args.find(a => a.startsWith('--uri='))?.slice(6) ?? file;

const liino = new Liino();
console.log('version:', liino.version);
console.log('lib:', liino.path);
console.log('diagnostics:', liino.diagnostics());
console.log('devices:', liino.devices());

const player = liino.create({ driver, volume: 0.3, app_name: 'Quaver Liino Probe' });
console.log('snapshot after create:', player.snapshot());
try {
  player.open(uri, { auto_play: false });
} catch (e) {
  console.log('open threw:', String(e));
}

for (let i = 0; i < 12; i++) {
  const raw = player.waitEvent(500);
  if (!raw) { console.log(`[${i}] (no event)`); continue; }
  const ev = JSON.parse(raw);
  console.log(`[${i}]`, ev.kind, JSON.stringify(ev.snapshot ?? null).slice(0, 260));
  if (ev.kind === 'failed' || ev.kind === 'ended') break;
}

console.log('clock:', player.clock());
console.log('level:', player.level());
player.close();
