import { useCallback, useEffect, useRef, useState } from 'react';

export interface Polled<T> {
  data: T | undefined;
  error: Error | null;
  loading: boolean;
  refresh: () => void;
}

/**
 * Poll a fetcher on an interval.
 *
 * Polling rather than a websocket because every number on these pages is a
 * recent sample, not an event stream: a pull job that finishes should show up
 * within a second or two, and a control plane that is down should show itself.
 * The fetcher receives a signal that fires on unmount, so a slow request does
 * not resolve into a component that no longer exists.
 */
export function usePoll<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  intervalMs = 0,
): Polled<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);

  // Held in a ref so changing the fetcher identity between renders does not
  // restart the timer: inline arrow functions are new on every render.
  const stable = useRef(fetcher);
  stable.current = fetcher;

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;

    const run = async () => {
      try {
        const next = await stable.current(controller.signal);
        if (cancelled) return;
        setData(next);
        setError(null);
      } catch (err) {
        if (cancelled || controller.signal.aborted) return;
        setError(err instanceof Error ? err : new Error(String(err)));
      } finally {
        if (!cancelled) setLoading(false);
      }
    };

    void run();
    if (intervalMs <= 0) return () => { cancelled = true; controller.abort(); };

    // Retry quickly while there is still nothing to show.
    //
    // A poll whose first attempt fails before the console has been told where
    // the control plane is — the ordering on a first visit to a fresh browser
    // — would otherwise show "not answering" for a full interval after the
    // address has been adopted and the service is in fact reachable. Thirty
    // seconds of confidently wrong is worse than a few extra requests.
    const hadData = data !== undefined;
    const delay = !hadData && error ? Math.min(intervalMs, 2000) : intervalMs;

    const timer = window.setInterval(run, delay);
    return () => {
      cancelled = true;
      controller.abort();
      window.clearInterval(timer);
    };
  }, [intervalMs, nonce, data, error]);

  const refresh = useCallback(() => setNonce((n) => n + 1), []);
  return { data, error, loading, refresh };
}

/** Bytes as a human string. GPU memory and model weights are both quoted this way. */
export function humanBytes(n: number, digits = 1): string {
  if (!n || n < 0) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let value = n;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i += 1;
  }
  return `${value.toFixed(i === 0 ? 0 : digits)} ${units[i]}`;
}

/** A duration as a compact relative age, for tables that refresh on a poll. */
export function humanAge(iso: string | undefined, now: number): string {
  if (!iso) return '—';
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return '—';
  const secs = Math.max(0, Math.round((now - then) / 1000));
  if (secs < 60) return `${secs}s`;
  if (secs < 3600) return `${Math.round(secs / 60)}m`;
  if (secs < 86400) return `${Math.round(secs / 3600)}h`;
  return `${Math.round(secs / 86400)}d`;
}

/**
 * Micro-units as money.
 *
 * Integer division on purpose: these are exact amounts and the number that
 * leaves this function is the one an operator puts on an invoice. Going through
 * a float renders 1026000 micro-units as 1.0260000000000001, which is not a
 * rounding artifact a reader dismisses — it reads as a broken invoice.
 */
export function humanMoney(micro: number, currency = '', fractionDigits?: number): string {
  const negative = micro < 0;
  const abs = Math.abs(micro);
  // Two decimals is the smallest that can express anything a currency has;
  // smaller amounts keep more digits so a near-zero line is not shown as zero.
  const digits = fractionDigits ?? (abs < 1_000_000 ? 4 : 2);
  const scale = 10 ** digits;
  const whole = Math.floor(abs / 1_000_000);
  const frac = Math.floor((abs % 1_000_000) / (1_000_000 / scale));
  const body = digits === 0 ? String(whole) : `${whole}.${String(frac).padStart(digits, '0')}`;
  return `${negative ? '-' : ''}${body}${currency ? ` ${currency}` : ''}`;
}

/** A fraction in millionths as a percentage with one decimal. */
export function humanPct(perMillion: number): string {
  return `${(perMillion / 10_000).toFixed(1)}%`;
}

/** GPU-seconds as GPU-hours, the unit a capacity conversation happens in. */
export function humanGpuSeconds(secs: number): string {
  if (secs <= 0) return '0 h';
  const hours = secs / 3600;
  if (hours < 100) return `${hours.toFixed(2)} h`;
  return `${Math.round(hours).toLocaleString()} h`;
}

/**
 * A budget window in the spelling an operator would have typed.
 *
 * The server sends seconds, because it must: it has one canonical duration and
 * Go's nanoseconds do not survive JSON as something a person can read. Its own
 * `windowText` is "720h0m0s", which is exact and unreadable, so the alias is
 * recovered here.
 *
 * A month is 30 days, not a calendar month — a rolling window that is 28 days
 * in February is not a budget a tenant can reason about. The calendar month is
 * for the cost pool's invoice, which is a different thing.
 */
export function humanWindow(seconds: number | undefined): string {
  if (!seconds || seconds <= 0) return '—';
  // Exact division at every level, never a rounded one. This string is also
  // the rule's delete id, so rounding is not cosmetic: 90 seconds printed as
  // "2m" would delete a rule that is not the one on screen and leave the
  // displayed one behind. The last branch handles anything that is not a whole
  // minute rather than pretending otherwise.
  if (seconds % 2592000 === 0) return `${seconds / 2592000}mo`;
  if (seconds % 604800 === 0) return `${seconds / 604800}w`;
  if (seconds % 86400 === 0) return `${seconds / 86400}d`;
  if (seconds % 3600 === 0) return `${seconds / 3600}h`;
  if (seconds % 60 === 0) return `${seconds / 60}m`;
  return `${seconds}s`;
}

/** The same window, spelled out for a tooltip. */
export function windowNote(seconds: number | undefined): string {
  if (!seconds || seconds <= 0) return '';
  if (seconds === 2592000) return '30 days, not a calendar month';
  if (seconds === 604800) return 'seven days';
  if (seconds === 86400) return '24 hours';
  return `${humanWindow(seconds)} of rolling window`;
}
