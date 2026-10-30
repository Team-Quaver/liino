/**
 * Transport options accepted by {@link LiinoPlayer.open}.
 */
export interface OpenOptions {
  start_ms?: number;
  auto_play?: boolean;
  format?: string;
  title?: string;
}

/** Library snapshot emitted with every event. */
export interface Snapshot {
  state: string;
  gen: number;
  position_ms: number;
  duration_ms: number;
  buffered_ms: number;
  latency_ms: number;
  latency_ns: number;
  volume: number;
  muted: boolean;
  driver: string;
  device: string;
  decoder: string;
  codec: string;
  live: boolean;
  underruns: number;
  clips: number;
  peak_l_db: number;
  peak_r_db: number;
}

/**
 * High-precision clock reading from {@link LiinoPlayer.clock}.
 *
 * `position_ms` estimates what the listener hears right now (buffer position
 * minus output latency). `monotonic_ns` is Go's monotonic clock in nanoseconds
 * for cross-process timing alignment.
 */
export interface PlayerClock {
  position_ms: number;
  buffer_pos_ms: number;
  latency_ms: number;
  latency_ns: number;
  monotonic_ns: number;
  frames: number;
  sample_rate: number;
}

/** Event from the native player. */
export interface PlayerEvent {
  kind:
    | 'loaded'
    | 'playing'
    | 'paused'
    | 'buffering'
    | 'ready'
    | 'ended'
    | 'stopped'
    | 'failed'
    | 'closed';
  name: string;
  gen: number;
  seek_gen: number;
  at: string;
  snapshot?: Snapshot;
}

/** Error code returned by libliino's C ABI. */
export const Code = {
  OK: 0,
  INVALID: -1,
  NOT_READY: -2,
  CLOSED: -3,
  INTERNAL: -4,
} as const;

/** Helpers shared by the FFI binding and application code. */
export function eventName(event: unknown): string {
  if (event && typeof event === 'object' && 'kind' in event) {
    return String((event as { kind: unknown }).kind);
  }
  return 'unknown';
}

/** True when the event is newer than `last`, using Liino's two generation counters. */
export function isNewGeneration(event: PlayerEvent | null, last: PlayerEvent | null): boolean {
  if (!event) return false;
  if (!last) return true;
  return event.gen !== last.gen || event.seek_gen !== last.seek_gen;
}
