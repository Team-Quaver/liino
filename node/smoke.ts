#!/usr/bin/env node
import { Liino } from './index.js';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const args = process.argv.slice(2);
const seconds = Number(args.find(a => a.startsWith('--seconds='))?.slice(10) ?? 1);
const driverArg = args.find(a => a.startsWith('--driver='))?.slice(9) ?? 'null';

const root = path.resolve(import.meta.dirname ?? '.', '../../testdata/tone-48000-stereo.ogg');
const liino = new Liino();
const player = liino.create({ driver: driverArg, volume: 0.5 });
player.open(root, { auto_play: true });

const loaded = player.waitEvent(5000);
if (!loaded) throw new Error('timed out waiting for load');
const event = JSON.parse(loaded) as { kind: string; snapshot?: { decoder: string } };
if (event.kind !== 'loaded') throw new Error(`unexpected event: ${event.kind}`);
if (!event.snapshot?.decoder) throw new Error('loaded event missing decoder');

player.play();
const deadline = Date.now() + seconds * 1000 + 1000;
let state = '';
while (Date.now() < deadline) {
  const raw = player.waitEvent(500);
  if (!raw) continue;
  const kind = (JSON.parse(raw) as { kind: string }).kind;
  if (kind === 'ended') break;
}
const snapshot = JSON.parse(player.snapshot() ?? '{}') as { state: string; driver: string; decoder: string };
state = snapshot.state;
if (!['playing', 'buffering', 'ended'].includes(state)) {
  throw new Error(`unexpected final state: ${state}`);
}
player.close();
console.log(`liino smoke ok: ${liino.version} driver=${snapshot.driver} decoder=${snapshot.decoder} state=${state}`);
