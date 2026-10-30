#!/usr/bin/env node
import { Liino } from './index.mjs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const args = process.argv.slice(2);
const seconds = Number(args.find(a => a.startsWith('--seconds='))?.slice(10) ?? 1);
const driverArg = args.find(a => a.startsWith('--driver='))?.slice(9) ?? 'null';

const root = path.resolve(fileURLToPath(import.meta.url), '../../testdata/tone-48000-stereo.ogg');
const liino = new Liino();
const player = liino.create({ driver: driverArg, volume: 0.5 });
player.open(root, { auto_play: true });

const loaded = player.waitEvent(5000);
if (!loaded) throw new Error('timed out waiting for load');
const event = JSON.parse(loaded);
if (event.kind !== 'loaded') throw new Error(`unexpected event: ${event.kind}`);
if (!event.snapshot?.decoder) throw new Error('loaded event missing decoder');

player.play();
const deadline = Date.now() + seconds * 1000 + 1000;
while (Date.now() < deadline) {
  const raw = player.waitEvent(500);
  if (!raw) continue;
  const kind = JSON.parse(raw).kind;
  if (kind === 'ended') break;
}
const snapshot = JSON.parse(player.snapshot());
if (!['playing', 'buffering', 'ended'].includes(snapshot.state)) {
  throw new Error(`unexpected final state: ${snapshot.state}`);
}
player.close();
console.log(`liino smoke ok: ${liino.version} driver=${snapshot.driver} decoder=${snapshot.decoder} state=${snapshot.state}`);
