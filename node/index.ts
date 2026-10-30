import { defaultLibrary } from './libliino.js';

export { Liino, LiinoError, LiinoPlayer, defaultLibrary } from './libliino.js';
export type { PlayerConfig } from './libliino.js';
export { Code, eventName, isNewGeneration } from './player.js';
export type { OpenOptions, PlayerClock, PlayerEvent, Snapshot } from './player.js';

/** Resolved path of the platform's libliino shared library. */
export const libraryPath: string = defaultLibrary();
