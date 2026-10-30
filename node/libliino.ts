import path from 'node:path';
import fs from 'node:fs';
import { createRequire } from 'node:module';
import type { IKoffiLib, KoffiFunction } from 'koffi';
import type { OpenOptions, PlayerClock, PlayerEvent, Snapshot } from './player.js';

const require = createRequire(import.meta.url);

type KoffiModule = typeof import('koffi');

const NAMES: Record<string, string | null> = {
  aix: null, darwin: 'libliino.dylib', freebsd: 'libliino.so',
  linux: 'libliino.so', openbsd: 'libliino.so', sunos: 'libliino.so',
  win32: 'liino.dll',
};

/** Resolve the packaged libliino shared library for the current platform. */
export function defaultLibrary(
  platform: string = process.platform,
  arch: string = process.arch,
): string {
  // Go uses amd64 while Node calls the same ABI x64.
  const goArch = arch === 'x64' ? 'amd64' : arch;
  // The library lives in Liino root build/. When running from TypeScript
  // source, import.meta.dirname is node/; when running compiled dist/, it is
  // node/dist/. Walk upward until we find the build directory.
  const here = import.meta.dirname ?? process.cwd();
  const dirs: string[] = [];
  let cursor = path.resolve(here);
  for (let i = 0; i < 4; i++) {
    const candidate = path.resolve(cursor, `build/${platform}-${goArch}`);
    if (fs.existsSync(candidate)) {
      dirs.push(candidate);
      break;
    }
    dirs.push(path.resolve(cursor, 'node', 'lib', `${platform}-${goArch}`));
    const parent = path.resolve(cursor, '..');
    if (parent === cursor) break;
    cursor = parent;
  }
  const name = NAMES[platform];
  if (!name) return dirs[0] ?? '';
  // Only the found build directory has the real library; other entries are
  // packaging fallbacks, so order matters: first existing dir wins.
  const candidates: string[] = [];
  for (const dir of dirs) {
    const direct = path.join(dir, name);
    if (fs.existsSync(direct)) {
      candidates.push(direct);
      break;
    }
  }
  // Development checkout fallback: the script currently only has a host build.
  for (const dir of dirs) {
    for (const entry of fs.existsSync(dir) ? fs.readdirSync(dir) : []) {
      if (/^libliino\.(so|dylib)$/.test(entry) || /^liino\.dll$/.test(entry)) {
        candidates.push(path.join(dir, entry));
      }
    }
  }
  return candidates[0] ?? name;
}

function loadKoffi(): KoffiModule {
  try {
    return require('koffi') as KoffiModule;
  } catch (err) {
    throw new Error(
      'liino-node requires the koffi package (Electron: add as an app dependency)',
      { cause: err },
    );
  }
}

const CODE = {
  OK: 0, INVALID: -1, NOT_READY: -2, CLOSED: -3, INTERNAL: -4,
} as const;

export class LiinoError extends Error {
  readonly code: number;

  constructor(code: number, message: string, detail?: string | null) {
    super([message, detail].filter(Boolean).join(': '));
    this.name = 'LiinoError';
    this.code = code;
  }
}

/** Serializable configuration accepted by {@link Liino.create}. */
export interface PlayerConfig {
  decoder?: string;
  driver?: string;
  device_id?: string;
  app_name?: string;
  stream_name?: string;
  ring_seconds?: number;
  preroll_ms?: number;
  period_ms?: number;
  fade_out_ms?: number;
  volume?: number;
  limiter?: boolean;
  limiter_db?: number;
  limiter_attack?: number;
  limiter_release?: number;
  eq?: Array<{ freq: number; q?: number; gain: number; shelf?: string }>;
}

interface OutputSlot {
  0: string | null;
  length: 1;
}

function readOutput(fn: KoffiFunction, args: unknown[]): string | null {
  // Disposable output types free their C string after Koffi converts it.
  const out: OutputSlot = [null];
  const code = fn(...args, out) as number;
  if (code === CODE.NOT_READY) return null;
  if (code !== CODE.OK) throw new LiinoError(code, 'Liino call failed');
  return out[0];
}

/**
 * FFI binding to libliino. One instance wraps one loaded shared library;
 * each {@link LiinoPlayer} created from it owns one native playback session.
 */
export class Liino {
  private readonly koffi: KoffiModule;
  private readonly lib: IKoffiLib;
  /** Platform-specific library path actually loaded. */
  readonly path: string;

  private readonly versionFn: KoffiFunction;
  private readonly diagnosticsFn: KoffiFunction;
  private readonly devicesFn: KoffiFunction;
  private readonly createFn: KoffiFunction;
  private readonly destroyFn: KoffiFunction;
  private readonly openFn: KoffiFunction;
  private readonly playFn: KoffiFunction;
  private readonly pauseFn: KoffiFunction;
  private readonly stopFn: KoffiFunction;
  private readonly seekFn: KoffiFunction;
  private readonly setVolumeFn: KoffiFunction;
  private readonly setMutedFn: KoffiFunction;
  private readonly setEQFn: KoffiFunction;
  private readonly setLimiterFn: KoffiFunction;
  private readonly setBypassFn: KoffiFunction;
  private readonly setDeviceFn: KoffiFunction;
  private readonly snapshotFn: KoffiFunction;
  private readonly clockFn: KoffiFunction;
  private readonly levelFn: KoffiFunction;
  private readonly nextEventFn: KoffiFunction;
  private readonly waitEventFn: KoffiFunction;

  constructor(library: string = defaultLibrary()) {
    this.koffi = loadKoffi();
    this.lib = this.koffi.load(library);
    this.path = library;
    // Koffi cannot free a pointer returned through char **. A disposable
    // string type makes Koffi call free() after converting the value to JS.
    this.koffi.disposable('LiinoHeapStr', 'char *');
    this.versionFn = this.lib.func('str! liino_version()');
    this.diagnosticsFn = this.lib.func('int liino_diagnostics(_Out_ LiinoHeapStr *out)');
    this.devicesFn = this.lib.func('int liino_devices(_Out_ LiinoHeapStr *out)');
    // Koffi cannot import C typedef names from the generated header, so every
    // opaque handle is declared as void * at the JS boundary.
    this.createFn = this.lib.func('void *liino_create(const char *config_json, _Out_ LiinoHeapStr *err)');
    this.destroyFn = this.lib.func('void liino_destroy(void *player)');
    this.openFn = this.lib.func('int liino_open(void *player, const char *uri, const char *options_json, _Out_ LiinoHeapStr *err)');
    const call1 = (name: string): KoffiFunction =>
      this.lib.func(`int ${name}(void *player, _Out_ LiinoHeapStr *err)`);
    this.playFn = call1('liino_play');
    this.pauseFn = call1('liino_pause');
    this.stopFn = call1('liino_stop');
    this.seekFn = this.lib.func('int liino_seek_ms(void *player, int64_t ms, _Out_ LiinoHeapStr *err)');
    this.setVolumeFn = this.lib.func('int liino_set_volume(void *player, double volume, _Out_ LiinoHeapStr *err)');
    this.setMutedFn = this.lib.func('int liino_set_muted(void *player, int muted, _Out_ LiinoHeapStr *err)');
    this.setEQFn = this.lib.func('int liino_set_eq(void *player, const char *bands_json, _Out_ LiinoHeapStr *err)');
    this.setLimiterFn = this.lib.func('int liino_set_limiter(void *player, int enabled, _Out_ LiinoHeapStr *err)');
    this.setBypassFn = this.lib.func('int liino_set_bypass(void *player, int bypass, _Out_ LiinoHeapStr *err)');
    this.setDeviceFn = this.lib.func('int liino_set_device(void *player, const char *device_id, _Out_ LiinoHeapStr *err)');
    this.snapshotFn = this.lib.func('int liino_snapshot_json(void *player, _Out_ LiinoHeapStr *out)');
    this.clockFn = this.lib.func('int liino_clock_json(void *player, _Out_ LiinoHeapStr *out)');
    this.levelFn = this.lib.func('int liino_level_json(void *player, _Out_ LiinoHeapStr *out)');
    this.nextEventFn = this.lib.func('int liino_next_event(void *player, _Out_ LiinoHeapStr *out)');
    this.waitEventFn = this.lib.func('int liino_wait_event(void *player, int timeout_ms, _Out_ LiinoHeapStr *out)');
  }

  get version(): string { return this.versionFn() as string; }

  diagnostics(): string | null { return readOutput(this.diagnosticsFn, []); }

  devices(): string | null { return readOutput(this.devicesFn, []); }

  create(config: PlayerConfig = {}): LiinoPlayer {
    const cfg = JSON.stringify(config);
    const err: OutputSlot = [null];
    const raw = this.createFn(cfg, err) as unknown;
    if (err[0]) throw new LiinoError(CODE.INVALID, 'liino_create failed', err[0]);
    if (!raw) throw new LiinoError(CODE.INTERNAL, 'liino_create returned no handle');
    return new LiinoPlayer(this, raw);
  }
}

/** One native playback session created by {@link Liino.create}. */
export class LiinoPlayer {
  #closed = false;
  #lib: Liino;
  #handle: unknown;

  constructor(lib: Liino, handle: unknown) {
    this.#lib = lib;
    this.#handle = handle;
  }

  #call(fn: KoffiFunction, ...args: unknown[]): void {
    if (this.#closed) throw new LiinoError(CODE.CLOSED, 'player closed');
    const err: OutputSlot = [null];
    const code = fn(...args, err) as number;
    if (code !== CODE.OK) throw new LiinoError(code, 'Liino call failed', err[0]);
  }

  open(uri: string, options: OpenOptions = {}): void {
    this.#call((this.#lib as unknown as { openFn: KoffiFunction }).openFn,
      this.#handle, uri, JSON.stringify(options));
  }
  play(): void { this.#transport('playFn'); }
  pause(): void { this.#transport('pauseFn'); }
  stop(): void { this.#transport('stopFn'); }

  #transport(name: 'playFn' | 'pauseFn' | 'stopFn'): void {
    const fn = (this.#lib as unknown as Record<string, KoffiFunction>)[name]!;
    this.#call(fn, this.#handle);
  }

  seek(ms: number): void {
    this.#call((this.#lib as unknown as { seekFn: KoffiFunction }).seekFn,
      this.#handle, Math.trunc(Number(ms) || 0));
  }
  setVolume(volume: number): void {
    this.#call((this.#lib as unknown as { setVolumeFn: KoffiFunction }).setVolumeFn,
      this.#handle, Number(volume) || 0);
  }
  setMuted(muted = true): void {
    this.#call((this.#lib as unknown as { setMutedFn: KoffiFunction }).setMutedFn,
      this.#handle, muted ? 1 : 0);
  }
  setEQ(bands: PlayerConfig['eq']): void {
    this.#call((this.#lib as unknown as { setEQFn: KoffiFunction }).setEQFn,
      this.#handle, JSON.stringify(bands ?? []));
  }
  setLimiter(enabled: boolean): void {
    this.#call((this.#lib as unknown as { setLimiterFn: KoffiFunction }).setLimiterFn,
      this.#handle, enabled ? 1 : 0);
  }
  setBypass(bypass: boolean): void {
    this.#call((this.#lib as unknown as { setBypassFn: KoffiFunction }).setBypassFn,
      this.#handle, bypass ? 1 : 0);
  }
  setDevice(deviceID?: string): void {
    this.#call((this.#lib as unknown as { setDeviceFn: KoffiFunction }).setDeviceFn,
      this.#handle, deviceID ?? '');
  }

  snapshot(): string | null {
    return readOutput((this.#lib as unknown as { snapshotFn: KoffiFunction }).snapshotFn, [this.#handle]);
  }
  clock(): string | null {
    return readOutput((this.#lib as unknown as { clockFn: KoffiFunction }).clockFn, [this.#handle]);
  }
  level(): string | null {
    return readOutput((this.#lib as unknown as { levelFn: KoffiFunction }).levelFn, [this.#handle]);
  }
  nextEvent(): string | null {
    return readOutput((this.#lib as unknown as { nextEventFn: KoffiFunction }).nextEventFn, [this.#handle]);
  }
  waitEvent(timeoutMS: number): string | null {
    return readOutput((this.#lib as unknown as { waitEventFn: KoffiFunction }).waitEventFn,
      [this.#handle, Math.trunc(Number(timeoutMS) || 0)]);
  }

  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    (this.#lib as unknown as { destroyFn: KoffiFunction }).destroyFn(this.#handle);
    this.#handle = null;
  }
}

// Re-export types so `import { Snapshot } from '@team-quaver/liino-node'` works.
export type { OpenOptions, PlayerClock, PlayerEvent, Snapshot };
