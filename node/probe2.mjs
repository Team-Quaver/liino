import { Liino } from './dist/index.js';
import path from 'node:path';
import process from 'node:process';

const file = path.resolve(import.meta.dirname, '../testdata/tone-48000-stereo.ogg');
const liino = new Liino();

for (const driver of ['null', '', 'pipewire']) {
  const p = liino.create({ driver, volume: 0.2 });
  try { p.open(file, { auto_play: false }); }
  catch (e) { console.log(driver || '<default>', 'open threw:', String(e)); continue; }
  const raw = p.waitEvent(3000);
  console.log('driver=%s ev=%s', driver || '<default>', raw);
  p.close();
}
