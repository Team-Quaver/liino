/**
 * Transport options accepted by {@link LiinoPlayer.open}.
 * @property {number} [start_ms]
 * @property {boolean} [auto_play]
 * @property {string} [format]
 * @property {string} [title]
 */
export {};

/**
 * Library snapshot emitted with every event.
 * @typedef {object} Snapshot
 * @property {string} state
 * @property {number} gen
 * @property {number} position_ms
 * @property {number} duration_ms
 * @property {number} buffered_ms
 * @property {number} volume
 * @property {boolean} muted
 * @property {string} driver
 * @property {string} device
 * @property {string} decoder
 * @property {string} codec
 * @property {boolean} live
 * @property {number} underruns
 * @property {number} clips
 * @property {number} peak_l_db
 * @property {number} peak_r_db
 */

/**
 * Event from the native player. `snapshot` is present except for a final
 * closed event emitted by {@link LiinoPlayer#close}.
 * @typedef {object} PlayerEvent
 * @property {'loaded'|'playing'|'paused'|'buffering'|'ready'|'ended'|'stopped'|'failed'|'closed'} kind
 * @property {string} name
 * @property {number} gen
 * @property {number} seek_gen
 * @property {string} at
 * @property {Snapshot=} snapshot
 */

/** Error code returned by libliino's C ABI. */
export const Code = {
  OK: 0,
  INVALID: -1,
  NOT_READY: -2,
  CLOSED: -3,
  INTERNAL: -4,
};

/**
 * Helpers shared by the FFI binding and application code.
 */
export function eventName(event) {
  return event?.kind ?? 'unknown';
}

/** True when the event is newer than `last`, using Liino's two generation counters. */
export function isNewGeneration(event, last) {
  if (!event) return false;
  if (!last) return true;
  return event.gen !== last.gen || event.seek_gen !== last.seek_gen;
}
