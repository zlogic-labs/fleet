import type { FormInstance } from 'antd';
import { App as AntApp } from 'antd';
import { useCallback, useEffect, useState } from 'react';

import { errorText } from '../api/client';

/** How long to wait before the first retry, until a read has ever succeeded. */
const retryMs = 2000;

/**
 * The shape every admin table on this console has: read a list, add a row,
 * delete a row, and tell the page above when the answer changed.
 *
 * Four panels did this by hand and the copies had already drifted -- one reset
 * its form and one did not, one told the parent about a deletion and one did
 * not, one tracked "has it loaded" and three did not. The point of the hook is
 * not fewer lines; it is that the next person editing one of those four cannot
 * make it behave differently from the other three without noticing.
 *
 * Not a generic data layer. It knows about antd forms and antd's message API
 * because that is what every caller here uses, and it refuses to guess at
 * anything else: the reader, the writer, the id and the labels all come in.
 */
export interface CrudOptions<T, V> {
  /** Reads the rows. Called again after every mutation. */
  read: () => Promise<T[]>;
  /**
   * Changes to this string re-read the list. Panels are keyed by the scope they
   * belong to, so this is the tenant or project id.
   */
  scope: string;
  /**
   * Re-reads on this interval, in milliseconds. Omit it and the list is read
   * once per scope change.
   *
   * The tenants panel polls because a tenant can be created by another operator
   * or by an API call, and an operator holding a provisioning page open should
   * see it. The panels below one do not, because everything under a tenant
   * changes only in response to what this operator just did.
   */
  pollMs?: number;
  create: (values: V) => Promise<unknown>;
  /**
   * Deletes a row. It receives the row rather than an id because the four
   * panels that use this do not agree on what identifies one: a project's id is
   * "tenant/name", and a budget rule's is the scope plus the dimension plus the
   * window, which the server only echoes back as a duration. Making each of
   * them build an id the hook then had to look up again put the row back in.
   */
  remove: (row: T) => Promise<unknown>;
  form: FormInstance<V>;
  /** Shown when the read fails. */
  readError: string;
  /**
   * What the panel says when there is genuinely nothing. It is a statement
   * about the state of the fleet, so it must never be used for a read that did
   * not happen: "no keys — nobody can call this project yet" is false when the
   * list never arrived.
   */
  empty: string;
  createError: string;
  removeError: string;
  /**
   * Called after a successful create or delete, for a panel whose parent caches
   * the same data (the tenant count on the Overview page reads the same rows).
   */
  onChanged?: () => void;
  /** Called after a delete, with the row that went, so a selection can be cleared. */
  onRemoved?: (row: T) => void;
}

export function useCrud<T, V>(opts: CrudOptions<T, V>) {
  const { message } = AntApp.useApp();
  const [rows, setRows] = useState<T[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [answered, setAnswered] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const {
    read, scope, form, onChanged, onRemoved, readError, createError, removeError, pollMs, empty,
    create: write, remove: erase,
  } = opts;

  const reload = useCallback(async () => {
    try {
      setRows(await read());
      setError(undefined);
      setAnswered(true);
    } catch (err) {
      // Kept, not just toasted: a panel whose list came back empty and a panel
      // whose list never arrived look identical in the table, and the console
      // has to be able to tell an operator which one it is.
      setError(err);
      // Not toasted until something has ever answered. Before the first
      // success the page is already showing a persistent alert that explains
      // the failure, and this read retries every couple of seconds until it
      // works -- a toast per attempt turns one problem into a stream of
      // dismissable copies of it.
      //
      // errorText, not the error itself: `${err}` on an ApiError prints
      // "ApiError: <message>", which puts a Go-style class name in front of
      // every message an operator reads.
      if (answered) message.error(`${readError}: ${errorText(err)}`);
    } finally {
      // Set even on failure: the empty text has to stop saying "loading" when
      // the panel is empty because nothing exists, not because nothing answered.
      setLoaded(true);
    }
  }, [read, message, readError, answered]);

  const [seen, setSeen] = useState<string>();
  if (seen !== scope) {
    setSeen(scope);
    setLoaded(false);
    void reload();
  }

  useEffect(() => {
    if (!pollMs) return;
    const timer = window.setInterval(() => void reload(), pollMs);
    return () => window.clearInterval(timer);
  }, [pollMs, reload]);

  // Retry until the first success, at a short interval and regardless of
  // pollMs.
  //
  // The console does not know the control plane's address on its first render:
  // it takes it from the gateway's first answer, one poll later. Every panel
  // therefore fires once against a guessed localhost, and on a deployment where
  // the two processes are on different hosts that first request fails. A panel
  // that only re-reads on its own interval would sit on that failure for the
  // whole interval -- 15 seconds for tenants, and forever for the three panels
  // that do not poll at all -- and the console would report "no control plane is
  // answering" about a control plane that is up.
  useEffect(() => {
    if (answered || !error) return;
    const timer = window.setTimeout(() => void reload(), retryMs);
    return () => window.clearTimeout(timer);
  }, [answered, error, retryMs, reload]);

  // The destructured writers rather than `opts` as the dependency: every caller
  // builds its options object inline, so depending on it would rebuild both
  // callbacks on every render and make them useless in a consumer's own deps.
  const create = useCallback(
    async (values: V) => {
      setBusy(true);
      try {
        await write(values);
        form.resetFields();
        await reload();
        onChanged?.();
      } catch (err) {
        message.error(`${createError}: ${errorText(err)}`);
      } finally {
        setBusy(false);
      }
    },
    [write, form, reload, onChanged, message, createError],
  );

  const remove = useCallback(
    async (row: T) => {
      try {
        await erase(row);
        await reload();
        onRemoved?.(row);
        onChanged?.();
      } catch (err) {
        message.error(`${removeError}: ${errorText(err)}`);
      }
    },
    [erase, reload, onRemoved, onChanged, message, removeError],
  );

  // Three states, not two. The four panels each derived this themselves and
  // each got it wrong in a different direction: one kept saying "loading" after
  // a read that had already failed, the other three announced "no keys" or "no
  // projects" about a list that never arrived. A panel whose read failed has
  // not established that anything is empty.
  const emptyText = !loaded ? 'loading' : error ? readError : empty;

  return { rows, setRows, loaded, answered, error, busy, emptyText, reload, create, remove };
}
