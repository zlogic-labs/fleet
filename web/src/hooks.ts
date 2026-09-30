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

    const timer = window.setInterval(run, intervalMs);
    return () => {
      cancelled = true;
      controller.abort();
      window.clearInterval(timer);
    };
  }, [intervalMs, nonce]);

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
