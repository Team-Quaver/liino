import path from 'node:path';
import fs from 'node:fs';
import os from 'node:os';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);

const NAMES = {
  aix: null, darwin: 'libliino.dylib', freebsd: 'libliino.so',
  linux: 'libliino.so', openbsd: 'libliino.so', sunos: 'libliino.so',
  win32: 'liino.dll',
};

export function defaultLibrary(platform = process.platform, arch = process.arch) {
  // Go uses amd64 while Node calls the same ABI x64.
  const goArch = arch === 'x64' ? 'amd64' : arch;
  const dirs = [
    path.resolve(import.meta.dirname, `../build/${platform}-${goArch}`),
    path.resolve(import.meta.dirname, 'lib', `${platform}-${goArch}`),
  ];
  const name = NAMES[platform];
  if (!name) return dirs[0];
  const candidates = [];
  for (const dir of dirs) candidates.push(path.join(dir, name));
  // Development checkout fallback: the script currently only has a host build.
  for (const dir of dirs) {
    for (const entry of fs.existsSync(dir) ? fs.readdirSync(dir) : []) {
      if (/^libliino\.(so|dylib)$/.test(entry) || /^liino\.dll$/.test(entry)) candidates.push(path.join(dir, entry));
    }
  }
  return candidates[0] ?? name;
}

function loadKoffi() {
  try {
    return require('koffi');
  } catch (err) {
    throw new Error('liino-node requires the koffi package (Electron: add as an app dependency)', { cause: err });
  }
}

const CODE = Object.freeze({
  OK: 0, INVALID: -1, NOT_READY: -2, CLOSED: -3, INTERNAL: -4,
});

export class LiinoError extends Error {
  constructor(code, message, detail) {
    super([message, detail].filter(Boolean).join(': '));
    this.name = 'LiinoError';
    this.code = code;
  }
}

const STR = 'char *';

function readOutput(fn, args) {
  // Disposable output types free their C string after Koffi converts it.
  const out = [null];
  const code = fn(...args, out);
  if (code === CODE.NOT_READY) return null;
  if (code !== CODE.OK) throw new LiinoError(code, 'Liino call failed');
  return out[0];
}

export class Liino {
  constructor(library = defaultLibrary()) {
    this.koffi = loadKoffi();
    this.lib = this.koffi.load(library);
    this.path = library;
    // Koffi cannot free a pointer returned through char **. A disposable
    // string type makes Koffi call free() after converting the value to JS.
    this.heapStr = this.koffi.disposable('LiinoHeapStr', 'char *');
    this.versionFn = this.lib.func('str! liino_version()');
    this.diagnosticsFn = this.lib.func('int liino_diagnostics(_Out_ LiinoHeapStr *out)');
    this.devicesFn = this.lib.func('int liino_devices(_Out_ LiinoHeapStr *out)');
    // Koffi cannot import C typedef names from the generated header, so every
    // opaque handle is declared as void * at the JS boundary.
    this.createFn = this.lib.func('void *liino_create(const char *config_json, _Out_ LiinoHeapStr *err)');
    this.destroyFn = this.lib.func('void liino_destroy(void *player)');
    this.openFn = this.lib.func('int liino_open(void *player, const char *uri, const char *options_json, _Out_ LiinoHeapStr *err)');
    const call1 = name => this.lib.func(`int ${name}(void *player, _Out_ LiinoHeapStr *err)`);
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

  get version() { return this.versionFn(); }

  diagnostics() { return readOutput(this.diagnosticsFn, []); }
  devices() { return readOutput(this.devicesFn, []); }

  create(config = {}) {
    const cfg = JSON.stringify(config ?? {});
    const err = [null];
    const raw = this.createFn(cfg, err);
    if (err[0]) throw new LiinoError(CODE.INVALID, 'liino_create failed', err[0]);
    if (!raw) throw new LiinoError(CODE.INTERNAL, 'liino_create returned no handle');
    return new LiinoPlayer(this, raw);
  }
}

export class LiinoPlayer {
  #closed = false;

  constructor(lib, handle) {
    this.lib = lib; this.handle = handle;
  }

  #call(fn, ...args) {
    if (this.#closed) throw new LiinoError(CODE.CLOSED, 'player closed');
    const err = [null];
    const code = fn(...args, err);
    if (code !== CODE.OK) throw new LiinoError(code, 'Liino call failed', err[0]);
  }

  open(uri, options = {}) { this.#call(this.lib.openFn, this.handle, uri, JSON.stringify(options ?? {})); }
  play() { this.#call(this.lib.playFn, this.handle); }
  pause() { this.#call(this.lib.pauseFn, this.handle); }
  stop() { this.#call(this.lib.stopFn, this.handle); }
  seek(ms) { this.#call(this.lib.seekFn, this.handle, Math.trunc(Number(ms) || 0)); }
  setVolume(volume) { this.#call(this.lib.setVolumeFn, this.handle, Number(volume) || 0); }
  setMuted(muted = true) { this.#call(this.lib.setMutedFn, this.handle, muted ? 1 : 0); }
  setEQ(bands) { this.#call(this.lib.setEQFn, this.handle, JSON.stringify(bands ?? [])); }
  setLimiter(enabled) { this.#call(this.lib.setLimiterFn, this.handle, enabled ? 1 : 0); }
  setBypass(bypass) { this.#call(this.lib.setBypassFn, this.handle, bypass ? 1 : 0); }
  setDevice(deviceID) { this.#call(this.lib.setDeviceFn, this.handle, deviceID ?? ''); }

  snapshot() { return readOutput(this.lib.snapshotFn, [this.handle]); }
  clock() { return readOutput(this.lib.clockFn, [this.handle]); }
  level() { return readOutput(this.lib.levelFn, [this.handle]); }
  nextEvent() { return readOutput(this.lib.nextEventFn, [this.handle]); }
  waitEvent(timeoutMS) { return readOutput(this.lib.waitEventFn, [this.handle, Math.trunc(Number(timeoutMS) || 0)]); }

  close() {
    if (this.#closed) return;
    this.#closed = true;
    this.lib.destroyFn(this.handle);
    this.handle = null;
  }
}
