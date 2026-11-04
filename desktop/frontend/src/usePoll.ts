import { useCallback, useEffect, useRef } from "react";
import { toast } from "sonner";

// The webview reports the page hidden while the window is hidden to the tray
// or minimised (WKWebView on macOS, WebKitGTK on Linux; on Windows Wails sets
// the WebView2 controller invisible on hide and on minimise), so polling the
// daemon can stop until the window is back.
const onScreen = () => document.visibilityState === "visible";

/**
 * Calls fn now and then intervalMs after each call settles, so two calls never
 * overlap. Paused while the page is hidden or `enabled` is false; on coming
 * back it calls at once if a tick was missed. fn and intervalMs are read at
 * each tick, so they may change between renders.
 *
 * Returns a function that calls fn right away, or right after the call in
 * flight, and resolves once that call has settled.
 */
export function usePoll(fn: () => unknown, intervalMs: number, enabled = true): () => Promise<void> {
  const opts = useRef({ fn, intervalMs, enabled });
  useEffect(() => {
    opts.current = { fn, intervalMs, enabled };
  });
  const ctl = useRef<{ now: () => Promise<void>; resume: () => void } | null>(null);

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    let busy = false; // a call is in flight
    let again = false; // now() was asked during it
    let dead = false;
    let last = 0; // when the last call settled
    let waiters: (() => void)[] = []; // now() callers waiting for a call to start
    const release = () => { const w = waiters; waiters = []; w.forEach((r) => r()); };
    const live = () => !dead && opts.current.enabled && onScreen();
    const schedule = (ms: number) => {
      clearTimeout(timer);
      timer = live() ? setTimeout(tick, Math.max(0, ms)) : undefined;
    };
    const tick = async () => {
      clearTimeout(timer);
      timer = undefined;
      if (busy) return;
      if (!live()) return release(); // nothing will run: don't keep callers waiting
      busy = true;
      const mine = waiters;
      waiters = [];
      try { await opts.current.fn(); } catch { /* fn reports its own failures */ }
      busy = false;
      last = Date.now();
      mine.forEach((r) => r());
      if (again) { again = false; void tick(); } else schedule(opts.current.intervalMs);
    };
    const now = () => new Promise<void>((resolve) => {
      waiters.push(resolve);
      if (busy) again = true;
      else void tick();
    });
    const resume = () => { if (!busy) schedule(last + opts.current.intervalMs - Date.now()); };
    document.addEventListener("visibilitychange", resume);
    ctl.current = { now, resume };
    void tick();
    return () => {
      dead = true;
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", resume);
      ctl.current = null;
      release();
    };
  }, []);

  const wasEnabled = useRef(enabled);
  useEffect(() => {
    if (enabled !== wasEnabled.current) {
      if (enabled) void ctl.current?.now();
      else ctl.current?.resume(); // drops the pending tick
    }
    wasEnabled.current = enabled;
  }, [enabled]);

  return useCallback(() => ctl.current?.now() ?? Promise.resolve(), []);
}

/**
 * Toasts a polling failure once, not on every tick: the next failure shows
 * only after ok() has seen a success.
 */
export function useFailureToast() {
  const shown = useRef(false);
  return useRef({
    ok: () => { shown.current = false; },
    fail: (message: string) => {
      if (shown.current) return;
      shown.current = true;
      toast.error(message);
    },
  }).current;
}
