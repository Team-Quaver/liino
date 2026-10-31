// Liino native 行为探针 #5：seek 之后还能不能继续推进/走到 ended？
// A) create 时 driver=null；B) create 时默认(pipewire) 再 setDevice("null")。
import { Liino } from './dist/index.js';
import path from 'node:path';

const file = path.resolve(import.meta.dirname, '../testdata/tone-48000-stereo.ogg');
const liino = new Liino();
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

async function run(label, cfg, device) {
  const p = liino.create({ ...cfg, volume: 0.2 });
  if (device) p.setDevice(device);
  const log = [];

  const drain = () => {
    for (let i = 0; i < 8; i++) {
      const raw = p.nextEvent();
      if (!raw) break;
      const ev = JSON.parse(raw);
      const s = ev.snapshot ?? {};
      log.push(`${ev.kind}/${s.state}@${Math.round(s.position_ms ?? -1)}`);
    }
  };

  p.open(file, { auto_play: true });
  for (let i = 0; i < 12; i++) { drain(); await wait(100); }
  const before = JSON.parse(p.snapshot()).state;
  log.push(`SEEK>`);
  p.seek(1200);
  for (let i = 0; i < 40; i++) { drain(); await wait(100); }
  const snap = JSON.parse(p.snapshot());
  console.log(`${label}: pre=${before} final=${snap.state}@${Math.round(snap.position_ms)} underruns=${snap.underruns} | ${log.join(' ')}`);
  p.close();
}

await run('A create-driver=null', { driver: 'null' }, '');
await run('B pipewire+setDevice(null)', { driver: '' }, 'null');
