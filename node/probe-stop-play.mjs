// Liino native 行为探针：play → stop → play 之后还能不能走到 ended？
// Quaver 的 stop = 「卸载当前流」，play 要能重挂；这里确认 Liino 自己的语义。
import { Liino } from './dist/index.js';
import path from 'node:path';

const file = path.resolve(import.meta.dirname, '../testdata/tone-48000-stereo.ogg');
const liino = new Liino();
const p = liino.create({ driver: 'null', volume: 0.2 });

const seen = [];
const drain = () => {
  for (let i = 0; i < 8; i++) {
    const raw = p.nextEvent();
    if (!raw) break;
    const ev = JSON.parse(raw);
    const s = ev.snapshot ?? {};
    seen.push(`${ev.kind}/${s.state}@${Math.round(s.position_ms ?? -1)}${s.error ? `!${s.error}` : ''}`);
  }
};
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

p.open(file, { auto_play: true });
for (let i = 0; i < 40; i++) { drain(); await wait(100); }
console.log('round1:', seen.join(' '));

seen.length = 0;
p.stop();
await wait(300); drain();
console.log('after stop:', seen.join(' ') || '(none)', 'snapshot=', p.snapshot());

seen.length = 0;
p.play();
for (let i = 0; i < 60; i++) { drain(); await wait(100); }
console.log('round2:', seen.join(' ') || '(none)');
console.log('final:', p.snapshot());

// 另一种口径：stop 后重新 open（引擎自己重挂流）
seen.length = 0;
p.open(file, { auto_play: true });
for (let i = 0; i < 40; i++) { drain(); await wait(100); }
console.log('reopen-after-stop:', seen.join(' ') || '(none)');
p.close();
