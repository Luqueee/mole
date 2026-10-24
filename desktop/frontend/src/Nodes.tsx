import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { ArrowRight, ChevronDown, Pencil, Play, Plus, RotateCw, Square, Trash2, X, Download, CornerDownLeft, Plug } from "lucide-react";
import type { NodeEntry, SSHHost } from "../bindings/github.com/Luqueee/mole/desktop";
import { api, type NodeInfo, type NodeList } from "./api";

const errMsg = (e: unknown) => (e instanceof Error ? e.message : String(e));
const validPort = (p: number) => Number.isInteger(p) && p >= 1 && p <= 65535;

/* ───────── state vocabulary ───────── */

type View = "checking" | "ready" | "no-agent" | "not-installed" | "unreachable";
type Probe = { info: NodeInfo | null; loading: boolean; at: number };
type Probes = Record<string, Probe | undefined>;

const TONE: Record<View, { text: string; bg: string; border: string; color: string; label: string }> = {
  checking: { text: "text-faint", bg: "bg-faint", border: "border-faint", color: "var(--ink-faint)", label: "CHECKING" },
  ready: { text: "text-live", bg: "bg-live", border: "border-live", color: "var(--live)", label: "READY" },
  "no-agent": { text: "text-warn", bg: "bg-warn", border: "border-warn", color: "var(--warn)", label: "NO AGENT" },
  "not-installed": { text: "text-warn", bg: "bg-warn", border: "border-warn", color: "var(--warn)", label: "NO MOLE" },
  unreachable: { text: "text-bad", bg: "bg-bad", border: "border-bad", color: "var(--bad)", label: "UNREACHABLE" },
};
const viewOf = (p?: Probe): View => (p?.info ? (p.info.state as View) : "checking");

const fmtMs = (ms: number) => (ms <= 0 ? "—" : ms < 10 ? `${ms.toFixed(1)}ms` : `${Math.round(ms)}ms`);
const osName = (os: string) => ({ darwin: "macOS", linux: "Linux", windows: "Windows", freebsd: "FreeBSD" } as Record<string, string>)[os] ?? os;
const semver = (v: string) => (v.match(/\d+/g) ?? []).slice(0, 3).map(Number);
function olderThan(a: string, b: string) {
  const x = semver(a), y = semver(b);
  if (!x.length || !y.length) return false;
  for (let i = 0; i < 3; i++) if ((x[i] ?? 0) !== (y[i] ?? 0)) return (x[i] ?? 0) < (y[i] ?? 0);
  return false;
}
const slug = (s: string) => s.toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^[^a-z0-9]+|-+$/g, "").slice(0, 32);

function useSize<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [size, setSize] = useState({ w: 0, h: 0 });
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setSize({ w: el.clientWidth, h: el.clientHeight }));
    ro.observe(el);
    setSize({ w: el.clientWidth, h: el.clientHeight });
    return () => ro.disconnect();
  }, []);
  return [ref, size] as const;
}

/* ───────── background jobs (update, console) ───────── */

function useJob() {
  const [text, setText] = useState("");
  const [running, setRunning] = useState(false);
  const [code, setCode] = useState<number | null>(null);
  const id = useRef(0);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true; // StrictMode mounts, unmounts and mounts again
    return () => { alive.current = false; };
  }, []);

  const run = useCallback(async (name: string, args: string[], opts: { header?: string; keep?: boolean; start?: () => Promise<number> } = {}) => {
    setRunning(true);
    setCode(null);
    setText((t) => (opts.keep ? t + (t && !t.endsWith("\n") ? "\n" : "") : "") + (opts.header ? opts.header + "\n" : ""));
    let exit = -1;
    try {
      const jid = await (opts.start ? opts.start() : api.jobStart(name, args));
      id.current = jid;
      let from = 0;
      for (;;) {
        const v = await api.job(jid, from);
        from = v.next;
        if (v.output && alive.current) setText((t) => t + v.output);
        if (v.done) {
          exit = v.code;
          if (v.error && alive.current) setText((t) => t + (t.endsWith("\n") || !t ? "" : "\n") + `[${v.error}]\n`);
          break;
        }
        await new Promise((r) => setTimeout(r, 250));
      }
    } catch (e) {
      if (alive.current) setText((t) => t + `${errMsg(e)}\n`);
    }
    if (alive.current) {
      setRunning(false);
      setCode(exit);
    }
    id.current = 0;
    return exit;
  }, []);
  const cancel = useCallback(() => { if (id.current) void api.jobCancel(id.current); }, []);
  const clear = useCallback(() => { setText(""); setCode(null); }, []);
  return { text, running, code, run, cancel, clear };
}

function Term({ text, running, empty }: { text: string; running?: boolean; empty?: string }) {
  const ref = useRef<HTMLPreElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (el && el.scrollHeight - el.scrollTop - el.clientHeight < 140) el.scrollTop = el.scrollHeight;
  }, [text]);
  return (
    <pre ref={ref} className="max-h-64 min-h-16 overflow-y-auto whitespace-pre-wrap break-all border border-line bg-black/40 p-3 text-[11px] leading-5 text-foreground select-text">
      {text || <span className="text-faint">{empty ?? "—"}</span>}
      {running && <span className="blink ml-0.5 inline-block h-3 w-1.5 translate-y-0.5 bg-hot" />}
    </pre>
  );
}

/* ───────── small UI pieces ───────── */

function Btn({ children, onClick, disabled, tone = "line", className = "", title }: { children: ReactNode; onClick?: () => void; disabled?: boolean; tone?: "line" | "hot" | "live" | "bad" | "warn"; className?: string; title?: string }) {
  const tones = {
    line: "border-line text-dim enabled:hover:bg-foreground enabled:hover:text-background",
    hot: "border-hot bg-hot text-background enabled:hover:brightness-110 font-bold",
    live: "border-live text-live enabled:hover:bg-live enabled:hover:text-background",
    bad: "border-bad text-bad enabled:hover:bg-bad enabled:hover:text-background",
    warn: "border-warn text-warn enabled:hover:bg-warn enabled:hover:text-background",
  };
  return (
    <button title={title} disabled={disabled} onClick={onClick} className={`no-drag flex items-center justify-center gap-2 border px-4 py-2 text-[11px] tracking-[0.2em] transition enabled:active:translate-y-px disabled:opacity-30 ${tones[tone]} ${className}`}>
      {children}
    </button>
  );
}

function Block({ title, hint, right, children }: { title: string; hint?: string; right?: ReactNode; children: ReactNode }) {
  return (
    <section className="border border-line bg-panel/60">
      <div className="flex items-center gap-3 border-b border-line px-4 py-2 text-[10px] tracking-[0.25em]">
        <span className="font-bold text-foreground">{title}</span>
        {hint && <span className="text-faint">{hint}</span>}
        <span className="ml-auto flex items-center gap-2">{right}</span>
      </div>
      <div className="p-4">{children}</div>
    </section>
  );
}

function Fact({ k, v, tone, wide }: { k: string; v: ReactNode; tone?: string; wide?: boolean }) {
  return (
    <div className={`min-w-0 ${wide ? "col-span-2" : ""}`}>
      <div className="text-[9px] tracking-[0.3em] text-faint">{k}</div>
      <div className={`mt-1 truncate text-xs select-text ${tone ?? "text-foreground"}`}>{v || <span className="text-faint">—</span>}</div>
    </div>
  );
}

function Stat({ label, value, tone }: { label: string; value: ReactNode; tone?: string }) {
  return (
    <div className="border border-line bg-black/20 px-3 py-2">
      <div className="text-[9px] tracking-[0.3em] text-faint">{label}</div>
      <div className={`mt-1 font-display text-2xl font-bold tabular-nums leading-none ${tone ?? ""}`}>{value}</div>
    </div>
  );
}

function MiniAdd({ placeholder, onAdd, pattern, busy }: { placeholder: string; onAdd: (v: string) => Promise<boolean> | boolean; pattern: RegExp; busy?: boolean }) {
  const [v, setV] = useState("");
  const go = async () => {
    if (!v) return;
    if (await onAdd(v)) setV("");
  };
  return (
    <div className="no-drag flex items-stretch border border-line">
      <input value={v} onChange={(e) => setV(e.target.value.replace(pattern, ""))} onKeyDown={(e) => e.key === "Enter" && go()} placeholder={placeholder} className="min-w-0 flex-1 bg-transparent px-3 py-2 text-xs tracking-[0.1em] text-foreground outline-none placeholder:text-faint select-text" />
      <button disabled={busy || !v} onClick={go} className="flex items-center gap-1 border-l border-line px-3 text-[11px] tracking-[0.2em] text-hot transition enabled:hover:bg-hot enabled:hover:text-background disabled:opacity-30">
        <Plus size={13} /> ADD
      </button>
    </div>
  );
}

/* ───────── the mesh map ───────── */

const NODE_W = 190;
const NODE_H = 68;

function MeshMap({ self, nodes, probes, selected, onSelect, onAdd }: { self: NodeList["self"]; nodes: NodeEntry[]; probes: Probes; selected: string | null; onSelect: (n: string) => void; onAdd: () => void }) {
  const [ref, { w, h }] = useSize<HTMLDivElement>();
  const ring = [...nodes.map((n) => n.name), ...(nodes.length < 9 ? ["\0add"] : [])];
  const cx = w / 2, cy = h / 2;
  const rx = Math.max(Math.min(w / 2 - NODE_W / 2 - 24, w * 0.38), 120);
  const ry = Math.max(h / 2 - NODE_H / 2 - 22, 50);
  const pos = (i: number) => {
    const a = (2 * Math.PI * i) / ring.length;
    return { x: cx + rx * Math.cos(a), y: cy + ry * Math.sin(a) };
  };
  return (
    <div ref={ref} className="grid-bg relative h-[360px] shrink-0 overflow-hidden border-b border-line">
      {w > 0 && (
        <svg className="absolute inset-0" width={w} height={h}>
          {ring.map((name, i) => {
            const p = pos(i);
            if (name === "\0add") return <line key="add" x1={cx} y1={cy} x2={p.x} y2={p.y} stroke="var(--ink-faint)" strokeWidth="1" strokeDasharray="2 6" opacity=".6" />;
            const pr = probes[name];
            const v = viewOf(pr);
            const info = pr?.info;
            const sel = selected === name;
            const color = TONE[v].color;
            const dur = Math.min(Math.max(0.9 + (info?.connectMs ?? 60) / 90, 1.1), 3.6);
            const path = `M${cx},${cy} L${p.x},${p.y}`;
            return (
              <g key={name} opacity={selected && !sel ? 0.55 : 1}>
                <line x1={cx} y1={cy} x2={p.x} y2={p.y} stroke={color} strokeWidth={sel ? 2 : 1} opacity={v === "ready" ? 0.55 : 0.8} strokeDasharray={v === "ready" ? undefined : v === "no-agent" || v === "not-installed" ? "8 5" : v === "unreachable" ? "2 7" : "3 5"} className={v === "checking" ? "blink" : undefined} />
                {v === "ready" && (
                  <>
                    <rect width="9" height="3" x="-4.5" y="-1.5" fill="var(--accent-hot)" style={{ filter: "drop-shadow(0 0 4px var(--accent-hot))" }}>
                      <animateMotion dur={`${dur}s`} repeatCount="indefinite" path={path} rotate="auto" />
                    </rect>
                    <rect width="9" height="3" x="-4.5" y="-1.5" fill="var(--live)" style={{ filter: "drop-shadow(0 0 4px var(--live))" }}>
                      <animateMotion dur={`${dur}s`} begin={`${dur / 2}s`} repeatCount="indefinite" path={`M${p.x},${p.y} L${cx},${cy}`} rotate="auto" />
                    </rect>
                  </>
                )}
              </g>
            );
          })}
        </svg>
      )}

      {/* latency on the links */}
      {w > 0 && ring.map((name, i) => {
        if (name === "\0add") return null;
        const pr = probes[name];
        if (viewOf(pr) !== "ready" || !pr?.info) return null;
        const p = pos(i);
        if (Math.hypot(p.x - cx, p.y - cy) < 240) return null; // a short link has no room: the node says it
        const lx = cx + (p.x - cx) * 0.5, ly = cy + (p.y - cy) * 0.5;
        return (
          <span key={name} style={{ left: lx, top: ly }} className="pointer-events-none absolute -translate-x-1/2 -translate-y-1/2 bg-background px-1.5 py-px text-[9px] tracking-[0.15em] text-dim tabular-nums">
            {fmtMs(pr.info.connectMs)}
          </span>
        );
      })}

      {/* this machine */}
      <div style={{ left: cx, top: cy }} className="absolute z-10 flex w-[184px] -translate-x-1/2 -translate-y-1/2 flex-col items-center bg-hot px-3 py-3 text-background shadow-[0_0_40px_-6px_var(--accent-hot)]">
        <div className="font-display text-lg font-extrabold tracking-[0.25em]">MOLE</div>
        <div className="mt-0.5 max-w-full truncate text-[11px] font-bold">{self.hostname || "this machine"}</div>
        <div className="text-[9px] tracking-[0.25em] opacity-70">{osName(self.os).toUpperCase()} · {self.arch.toUpperCase()}</div>
        <div className="mt-1.5 border border-background/60 px-1.5 py-px text-[8px] tracking-[0.3em]">THIS MACHINE</div>
      </div>

      {/* the nodes */}
      {w > 0 && ring.map((name, i) => {
        const p = pos(i);
        const base = { left: p.x, top: p.y, width: NODE_W, height: NODE_H };
        if (name === "\0add") {
          return (
            <button key="add" onClick={onAdd} style={base} className="no-drag group absolute z-10 flex -translate-x-1/2 -translate-y-1/2 flex-col items-center justify-center gap-1 border border-dashed border-faint bg-background/70 text-faint transition hover:border-hot hover:text-hot">
              <Plus size={16} />
              <span className="text-[9px] tracking-[0.3em]">{nodes.length ? "ADD NODE" : "ADD YOUR FIRST NODE"}</span>
            </button>
          );
        }
        const n = nodes.find((x) => x.name === name)!;
        const pr = probes[name];
        const v = viewOf(pr);
        const info = pr?.info;
        const sel = selected === name;
        return (
          <button key={name} onClick={() => onSelect(name)} style={{ ...base, animationDelay: `${i * 60}ms` }} className={`no-drag rise absolute z-10 -translate-x-1/2 -translate-y-1/2 border bg-panel px-3 py-2 text-left transition ${sel ? "border-hot shadow-[0_0_28px_-8px_var(--accent-hot)]" : `${TONE[v].border}/50 hover:border-foreground`} ${selected && !sel ? "opacity-70" : ""}`}>
            <div className="flex items-center gap-2">
              <span className={`size-2 shrink-0 ${TONE[v].bg} ${v === "checking" || pr?.loading ? "blink" : ""}`} />
              <span className="truncate font-display text-sm font-bold">{name}</span>
              {n.implicit && <span className="ml-auto text-[8px] tracking-[0.2em] text-faint">CONFIG</span>}
            </div>
            <div className="mt-0.5 truncate text-[10px] text-dim">{n.target}</div>
            <div className={`mt-1 truncate text-[9px] tracking-[0.2em] ${TONE[v].text}`}>
              {TONE[v].label}
              {v === "ready" && info && <span className="text-dim"> · v{info.version}{info.daemon?.running ? " · UP" : " · DOWN"}</span>}
              {v === "no-agent" && info?.version && <span className="text-dim"> · v{info.version}</span>}
            </div>
          </button>
        );
      })}

      <div className="pointer-events-none absolute bottom-2 left-4 text-[9px] tracking-[0.25em] text-faint">ANY NODE CAN MANAGE ANY OTHER · mole -n NODE &lt;command&gt;</div>
    </div>
  );
}

/* ───────── add / edit ───────── */

function NodeForm({ initial, existing, onDone, onCancel }: { initial?: NodeEntry; existing: string[]; onDone: (name: string, install?: boolean) => void; onCancel: () => void }) {
  const editing = !!initial;
  const [name, setName] = useState(initial?.name ?? "");
  const [target, setTarget] = useState(initial?.target ?? "");
  const [port, setPort] = useState(initial?.sshPort ? String(initial.sshPort) : "");
  const [bin, setBin] = useState(initial?.bin ?? "");
  const [insecure, setInsecure] = useState(initial?.insecure ?? false);
  const [check, setCheck] = useState(!editing);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [hosts, setHosts] = useState<SSHHost[]>([]);
  const [advanced, setAdvanced] = useState(!!(initial?.sshPort || initial?.bin || initial?.insecure));
  useEffect(() => { api.sshHosts().then(setHosts).catch(() => {}); }, []);

  const pick = (h: SSHHost) => {
    setTarget(h.alias);
    if (!editing && !name) setName(slug(h.alias));
  };
  const nameTaken = !editing && existing.includes(name);
  const nameOk = /^[a-z0-9][a-z0-9_-]{0,31}$/.test(name);
  const portNum = port === "" ? 0 : Number(port);
  const valid = nameOk && !nameTaken && target.trim() !== "" && (port === "" || validPort(portNum));

  const [noMole, setNoMole] = useState(false);
  const submit = async (installAfter = false) => {
    if (!valid || busy) return;
    setBusy(true);
    setError("");
    setNoMole(false);
    try {
      await api.nodeAdd({ name, target: target.trim(), sshPort: portNum, bin: bin.trim(), insecure, check: check && !installAfter, replace: editing });
      toast.success(editing ? `${name} updated` : `${name} added`);
      onDone(name, installAfter);
    } catch (e) {
      const msg = errMsg(e);
      setError(msg);
      // The machine answers but has no mole: offer to put it there.
      setNoMole(!editing && /not installed/i.test(msg));
    } finally {
      setBusy(false);
    }
  };
  const field = "no-drag w-full border border-line bg-transparent px-3 py-2.5 text-xs text-foreground outline-none transition placeholder:text-faint focus:border-hot select-text";
  const label = "mb-1.5 block text-[9px] tracking-[0.3em] text-faint";
  return (
    <div className="rise border-b border-hot/60 bg-hot/[0.03]">
      <div className="flex items-center gap-3 border-b border-line px-5 py-2 text-[10px] tracking-[0.25em]">
        <span className="font-bold text-hot">{editing ? `EDIT ${initial!.name.toUpperCase()}` : "NEW NODE"}</span>
        <span className="text-faint">A MACHINE YOU CAN REACH OVER SSH, WITH MOLE INSTALLED</span>
        <button aria-label="Close" onClick={onCancel} className="no-drag ml-auto text-faint transition hover:text-foreground"><X size={16} /></button>
      </div>
      <div className="grid grid-cols-[1fr_1.6fr] gap-5 px-5 py-4">
        <div>
          <label className={label}>NAME</label>
          <input disabled={editing} value={name} onChange={(e) => setName(e.target.value.toLowerCase().replace(/[^a-z0-9_-]/g, ""))} placeholder="devlabs" maxLength={32} className={`${field} font-display text-base font-bold disabled:opacity-50`} autoFocus={!editing} />
          <div className={`mt-1 h-3 text-[9px] tracking-[0.15em] ${nameTaken ? "text-bad" : "text-faint"}`}>{nameTaken ? "ALREADY REGISTERED" : "a-z 0-9 - _ · used in: mole -n NAME"}</div>
        </div>
        <div>
          <label className={label}>SSH TARGET</label>
          <input value={target} onChange={(e) => setTarget(e.target.value)} onKeyDown={(e) => e.key === "Enter" && submit()} placeholder="user@host[:port]  or  an alias of ~/.ssh/config" className={field} autoFocus={editing} />
          <div className="mt-1 h-3 text-[9px] tracking-[0.15em] text-faint">uses your ssh agent and keys, like the primary remote</div>
        </div>
      </div>

      {hosts.length > 0 && (
        <div className="px-5 pb-3">
          <div className={label}>FROM YOUR ~/.ssh/config</div>
          <div className="flex flex-wrap gap-2">
            {hosts.map((h) => (
              <button key={h.alias} onClick={() => pick(h)} className={`no-drag border px-3 py-1.5 text-left transition hover:border-hot ${target === h.alias ? "border-hot bg-hot/10" : "border-line"}`}>
                <div className="text-xs font-bold">{h.alias}</div>
                <div className="text-[9px] text-dim">{[h.user && `${h.user}@`, h.hostName, h.port ? `:${h.port}` : ""].filter(Boolean).join("")}</div>
              </button>
            ))}
          </div>
        </div>
      )}

      <div className="px-5 pb-3">
        <button onClick={() => setAdvanced((a) => !a)} className="no-drag flex items-center gap-1.5 text-[10px] tracking-[0.25em] text-dim transition hover:text-foreground">
          <ChevronDown size={12} className={`transition ${advanced ? "" : "-rotate-90"}`} /> ADVANCED
        </button>
        {advanced && (
          <div className="rise mt-3 grid grid-cols-[120px_1fr_auto] items-start gap-5">
            <div>
              <label className={label}>SSH PORT</label>
              <input value={port} onChange={(e) => setPort(e.target.value.replace(/\D/g, ""))} placeholder="22" className={field} />
            </div>
            <div>
              <label className={label}>MOLE ON THE NODE</label>
              <input value={bin} onChange={(e) => setBin(e.target.value)} placeholder="mole  (looked up in ~/.local/bin and the usual places)" className={field} />
            </div>
            <div>
              <label className={label}>HOST KEY</label>
              <button onClick={() => setInsecure((i) => !i)} className={`no-drag flex w-full items-center gap-2 border px-3 py-2.5 text-[11px] tracking-[0.15em] transition ${insecure ? "border-bad text-bad" : "border-line text-dim hover:text-foreground"}`}>
                <span className={`size-2 ${insecure ? "bg-bad" : "bg-live"}`} /> {insecure ? "NOT VERIFIED (UNSAFE)" : "VERIFIED"}
              </button>
            </div>
          </div>
        )}
      </div>

      {error && !noMole && <div className="mx-5 mb-3 whitespace-pre-wrap border border-bad bg-bad/10 px-3 py-2 text-[11px] text-bad select-text">{error}</div>}
      {noMole && (
        <div className="mx-5 mb-3 flex items-center gap-4 border border-warn bg-warn/10 px-4 py-3">
          <div className="flex-1">
            <div className="text-[10px] tracking-[0.3em] text-warn">MOLE IS NOT INSTALLED ON THAT MACHINE</div>
            <div className="mt-1 text-xs text-dim">It answers over SSH. Add it and install mole there now? It is built on the machine, and Go is downloaded if it has none.</div>
          </div>
          <Btn tone="hot" disabled={busy} onClick={() => void submit(true)} className="shrink-0"><Download size={13} /> ADD AND INSTALL MOLE</Btn>
        </div>
      )}

      <div className="flex items-center gap-4 border-t border-line px-5 py-3">
        <button onClick={() => setCheck((c) => !c)} className="no-drag flex items-center gap-2 text-[11px] tracking-[0.15em] text-dim transition hover:text-foreground">
          <span className={`flex size-4 items-center justify-center border ${check ? "border-live bg-live text-background" : "border-faint"}`}>{check && "✓"}</span>
          CHECK THE CONNECTION FIRST
        </button>
        <span className="text-[10px] text-faint">{check ? "refuses a node that does not answer" : "registers it without contacting it"}</span>
        <div className="ml-auto flex gap-2">
          <Btn onClick={onCancel}>CANCEL</Btn>
          <Btn tone="hot" disabled={!valid || busy} onClick={() => void submit()} className="min-w-36">
            {busy ? (check ? "CONTACTING…" : "SAVING…") : editing ? "SAVE" : "ADD NODE"}
          </Btn>
        </div>
      </div>
    </div>
  );
}

/* ───────── one node, in detail ───────── */

const QUICK = ["status", "ports list", "logs", "doctor", "inbox", "version"];

function NodeDetail({ entry, probe, selfVersion, reprobe, onEdit, onRemoved, autoInstall, onAutoInstalled }: { entry: NodeEntry; probe?: Probe; selfVersion: string; reprobe: () => Promise<void>; onEdit: () => void; onRemoved: () => void; autoInstall?: boolean; onAutoInstalled?: () => void }) {
  const info = probe?.info ?? null;
  const name = entry.name;
  const d = info?.daemon ?? null;
  const [busy, setBusy] = useState("");
  const [confirm, setConfirm] = useState(false);
  const [branch, setBranch] = useState("");
  const update = useJob();
  const cons = useJob();
  const [cmd, setCmd] = useState("");
  const hist = useRef<string[]>([]);
  const histAt = useRef(-1);

  const act = async (key: string, label: string, fn: () => Promise<unknown>) => {
    setBusy(key);
    try {
      await fn();
      toast.success(`${name}: ${label}`);
    } catch (e) {
      toast.error(errMsg(e));
    } finally {
      setBusy("");
      void reprobe();
    }
  };

  const runInstall = async () => {
    const b = branch.trim();
    if (b && !/^[A-Za-z0-9][A-Za-z0-9._/-]*$/.test(b)) return toast.error("That is not a branch name");
    const code = await update.run(name, [], { header: `$ mole node install ${name}${b ? ` -branch ${b}` : ""}`, start: () => api.jobInstall(name, b) });
    if (code === 0) toast.success(`mole installed on ${name}`);
    else if (code !== -1) toast.error(`the install failed on ${name} (exit ${code})`);
    void reprobe();
  };
  const autoRan = useRef(false);
  useEffect(() => {
    if (!autoInstall || autoRan.current) return;
    autoRan.current = true;
    onAutoInstalled?.();
    void runInstall();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoInstall]);

  const runUpdate = async () => {
    const b = branch.trim();
    if (b && !/^[A-Za-z0-9][A-Za-z0-9._/-]*$/.test(b)) return toast.error("That is not a branch name");
    const code = await update.run(name, b ? ["update", "-branch", b] : ["update"], { header: `$ mole -n ${name} update${b ? ` -branch ${b}` : ""}` });
    if (code === 0) toast.success(`${name} updated${b ? ` to ${b}` : ""}: restart its daemon to use it`);
    else if (code !== -1) toast.error(`update failed on ${name} (exit ${code})`);
    void reprobe();
  };

  const runCmd = async (line: string) => {
    const text = line.trim();
    if (!text || cons.running) return;
    let args: string[];
    try { args = await api.splitCommand(text); } catch (e) { return toast.error(errMsg(e)); }
    if (args[0] === "mole") args = args.slice(1);
    if (!args.length) return;
    hist.current = [text, ...hist.current.filter((h) => h !== text)].slice(0, 30);
    histAt.current = -1;
    setCmd("");
    await cons.run(name, args, { header: `$ mole -n ${name} ${args.join(" ")}`, keep: true });
  };

  const olderThanSelf = !!info?.version && !!selfVersion && olderThan(info.version, selfVersion);
  const needsUpdate = info?.state === "no-agent";
  const manageable = info?.state === "ready" && !!d;

  return (
    <div className="rise space-y-4 border-b border-line bg-black/20 px-5 py-5">
      {/* header actions */}
      <div className="flex items-center gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-baseline gap-3">
            <span className="font-display text-2xl font-bold">{name}</span>
            <span className="truncate text-xs text-dim select-text">{entry.target}{entry.sshPort ? ` · ssh ${entry.sshPort}` : ""}</span>
          </div>
          {entry.implicit && <div className="mt-1 text-[10px] tracking-[0.2em] text-faint">THE CONFIG'S OWN remote: · NOT IN THE NODE REGISTRY</div>}
        </div>
        <Btn disabled={probe?.loading} onClick={() => void reprobe()}><RotateCw size={12} className={probe?.loading ? "animate-spin" : ""} /> PING</Btn>
        {!entry.implicit && <Btn onClick={onEdit}><Pencil size={12} /> EDIT</Btn>}
        {!entry.implicit && (confirm ? (
          <div className="flex">
            <Btn tone="bad" disabled={busy === "rm"} onClick={() => act("rm", "removed", async () => { await api.nodeRemove(name); onRemoved(); })}><Trash2 size={12} /> REALLY REMOVE</Btn>
            <Btn className="-ml-px" onClick={() => setConfirm(false)}>NO</Btn>
          </div>
        ) : (
          <Btn tone="bad" onClick={() => setConfirm(true)}><Trash2 size={12} /> REMOVE</Btn>
        ))}
      </div>

      {info?.state === "unreachable" && (
        <div className="border border-bad bg-bad/10 px-4 py-3">
          <div className="text-[10px] tracking-[0.3em] text-bad">CANNOT REACH THIS NODE</div>
          <div className="mt-1.5 whitespace-pre-wrap text-xs text-foreground select-text">{info.error || "no answer"}</div>
          <div className="mt-2 text-[11px] text-dim">Check that <span className="text-foreground">ssh {entry.target}</span> works from a terminal, that the host key is known, and that mole is installed there (or set its path in EDIT).</div>
        </div>
      )}

      {info?.state === "not-installed" && (
        <Block title="INSTALL MOLE" hint="THE NODE ANSWERS, BUT HAS NO MOLE" right={update.running ? <Btn tone="bad" onClick={update.cancel}><X size={11} /> CANCEL</Btn> : undefined}>
          <div className="text-xs leading-5 text-dim">
            mole is built on that machine by the official installer, over SSH. It needs <span className="text-foreground">git</span> and <span className="text-foreground">curl</span> or <span className="text-foreground">wget</span> there; if Go is missing, the installer downloads it (checked against its published checksum) into <span className="text-foreground">~/.cache/mole/go</span>, without touching the system.
          </div>
          <div className="mt-3 flex items-stretch gap-3">
            <div className="no-drag flex flex-1 items-center border border-line">
              <span className="px-3 text-[10px] tracking-[0.25em] text-faint">BRANCH</span>
              <input value={branch} onChange={(e) => setBranch(e.target.value)} placeholder="latest main  ·  or a branch to try" className="min-w-0 flex-1 bg-transparent py-2.5 pr-3 text-xs text-foreground outline-none placeholder:text-faint select-text" />
            </div>
            <Btn tone="hot" disabled={update.running} onClick={runInstall} className="min-w-44"><Download size={13} /> {update.running ? "INSTALLING…" : "INSTALL MOLE"}</Btn>
          </div>
          {(update.text || update.running) && <div className="mt-3"><Term text={update.text} running={update.running} /></div>}
        </Block>
      )}

      {needsUpdate && (
        <div className="border border-warn bg-warn/10 px-4 py-3 text-xs text-warn">
          Its mole{info?.version ? ` (v${info.version})` : ""} answers, but is too old to be managed from here. Update it below and its details will appear.
        </div>
      )}

      {info && info.state !== "unreachable" && info.state !== "not-installed" && (
        <Block title="IDENTITY" hint={info.state === "ready" ? "REPORTED BY THE NODE'S AGENT" : "LIMITED: NO AGENT"}>
          <div className="grid grid-cols-4 gap-x-6 gap-y-4">
            <Fact k="HOST" v={info.hostname} />
            <Fact k="SYSTEM" v={info.os ? `${osName(info.os)} · ${info.arch}` : ""} />
            <Fact k="MOLE" v={info.version ? `v${info.version}` : ""} tone={olderThanSelf ? "text-warn" : undefined} />
            <Fact k="BUILT FROM" v={info.build} />
            <Fact k="CONNECT" v={fmtMs(info.connectMs)} />
            <Fact k="AGENT ROUND TRIP" v={info.state === "ready" ? fmtMs(info.roundTripMs) : ""} />
            <Fact k="CONFIG" v={d?.configPath} wide />
          </div>
          {olderThanSelf && <div className="mt-3 text-[11px] text-warn">Older than this machine (v{selfVersion}). Update it below to keep both in step.</div>}
        </Block>
      )}

      {manageable && d && (
        <>
          <Block
            title="DAEMON"
            hint={d.running ? "FORWARDING ON THAT MACHINE" : "NOT RUNNING"}
            right={
              <>
                <Btn tone="live" title={d.remote ? undefined : "this node has no remote configured"} disabled={d.running || !!busy || !d.remote} onClick={() => act("start", "started", () => api.nodeStart(name))}><Play size={11} /> {busy === "start" ? "STARTING…" : "START"}</Btn>
                <Btn disabled={!!busy} onClick={() => act("restart", "restarted", () => api.nodeRestart(name))}><RotateCw size={11} /> {busy === "restart" ? "RESTARTING…" : "RESTART"}</Btn>
                <Btn tone="bad" disabled={!d.running || !!busy} onClick={() => act("stop", "stopped", () => api.nodeStop(name))}><Square size={11} /> {busy === "stop" ? "STOPPING…" : "STOP"}</Btn>
              </>
            }
          >
            <div className="grid grid-cols-5 gap-3">
              <Stat label="STATE" value={d.running ? "UP" : "DOWN"} tone={d.running ? "text-live" : "text-bad"} />
              <Stat label="UPTIME" value={d.running ? d.uptime || "—" : "—"} />
              <Stat label="ACTIVE" value={d.activeConns} tone={d.activeConns ? "text-live" : undefined} />
              <Stat label="TOTAL" value={d.totalConns.toLocaleString("en")} />
              <Stat label="FAILED" value={d.failedDials.toLocaleString("en")} tone={d.failedDials ? "text-bad" : undefined} />
            </div>
            {!d.remote && (
              <div className="mt-4 border border-warn bg-warn/10 px-4 py-3 text-xs text-warn">
                This node has no <span className="font-bold">remote:</span> in its config, so its daemon has nothing to connect to and START would exit at once. A daemon forwards ports to <em>another</em> machine over SSH: on that node, run <span className="font-bold">mole init</span> (or add <span className="font-bold">remote: user@host</span> to its config) naming the machine it should reach, e.g. this one.
              </div>
            )}
            <div className="mt-4 grid grid-cols-4 gap-x-6 gap-y-3">
              <Fact k="ITS PRIMARY REMOTE" v={d.remote} />
              <Fact k="ADMIN" v={d.adminAddr} />
              <Fact k="RECEIVES FILES" v={d.receive ? `YES · ${d.accept === "auto" ? "SAVED AUTOMATICALLY" : "ASKS FIRST"}` : "NO"} tone={d.receive ? "text-live" : "text-dim"} />
              <Fact k="INBOX" v={d.inbox} />
            </div>
          </Block>

          <div className="grid grid-cols-2 gap-4">
            <Block title="PULL" hint="ITS PORTS (-L) · DISCOVERY LIST" right={<span className="text-faint">{d.ports.length} FORWARDED</span>}>
              <div className="flex min-h-9 flex-wrap gap-2">
                {d.discoverPorts.map((p) => {
                  const live = d.ports.includes(p);
                  return (
                    <span key={p} className={`group flex items-center gap-2 border px-2.5 py-1.5 text-xs tabular-nums ${live ? "border-live/60 text-foreground" : "border-line text-dim"}`}>
                      <span className={`size-1.5 ${live ? "bg-live" : "bg-faint"}`} />
                      {p}
                      <button aria-label={`Remove ${p}`} disabled={!!busy} onClick={() => act(`rp${p}`, `− ${p}`, () => api.nodePortRemove(name, p))} className="text-faint transition hover:text-bad"><X size={12} /></button>
                    </span>
                  );
                })}
                {d.discoverPorts.length === 0 && <span className="py-1.5 text-[11px] text-faint">NO DISCOVERY PORTS{d.autoDiscover ? " · AUTO-DISCOVER ON" : ""}</span>}
                {d.ports.filter((p) => !d.discoverPorts.includes(p)).map((p) => (
                  <span key={`f${p}`} title="forwarded, not in its discovery list" className="flex items-center gap-2 border border-dashed border-live/40 px-2.5 py-1.5 text-xs tabular-nums text-dim">
                    <span className="size-1.5 bg-live" />{p}
                  </span>
                ))}
              </div>
              <div className="mt-3">
                <MiniAdd busy={!!busy} placeholder="ADD DISCOVERY PORT" pattern={/\D/g} onAdd={async (v) => {
                  const p = Number(v);
                  if (!validPort(p)) { toast.error("Port must be 1–65535"); return false; }
                  await act(`ap${p}`, `+ ${p}`, () => api.nodePortAdd(name, p));
                  return true;
                }} />
              </div>
            </Block>

            <Block title="PUSH" hint="PORTS OF THAT MACHINE ON ITS REMOTE (-R)" right={<span className="text-faint">{d.push.length}</span>}>
              <div className="min-h-9 space-y-1.5">
                {d.push.map((p) => (
                  <div key={p.local} className="flex items-center gap-3 border border-line px-3 py-1.5 text-xs">
                    <span className={`size-1.5 ${p.state === "active" ? "bg-warn" : p.state === "failed" ? "bg-bad" : "bg-faint"}`} />
                    <span className="tabular-nums">{p.local}</span>
                    <ArrowRight size={12} className="text-warn" />
                    <span className="tabular-nums text-dim">{p.remote}</span>
                    {p.state === "failed" && p.error && <span className="truncate text-[10px] text-bad">{p.error}</span>}
                    <button aria-label={`Remove push ${p.local}`} disabled={!!busy} onClick={() => act(`rm${p.local}`, `− push ${p.local}`, () => api.nodePushRemove(name, p.local))} className="ml-auto text-faint transition hover:text-bad"><X size={12} /></button>
                  </div>
                ))}
                {d.push.length === 0 && <div className="py-1.5 text-[11px] text-faint">NOTHING PUSHED{d.running ? "" : " · DAEMON DOWN"}</div>}
              </div>
              <div className="mt-3">
                <MiniAdd busy={!!busy} placeholder="PUSH LOCAL[:REMOTE]" pattern={/[^\d:]/g} onAdd={async (v) => {
                  const m = /^(\d+)(?::(\d+))?$/.exec(v);
                  const local = Number(m?.[1]);
                  const remote = m?.[2] ? Number(m[2]) : 0;
                  if (!m || !validPort(local) || (remote !== 0 && !validPort(remote))) { toast.error("Use LOCAL or LOCAL:REMOTE, ports 1–65535"); return false; }
                  await act(`ps${local}`, `← ${local}${remote ? `:${remote}` : ""}`, () => api.nodePushAdd(name, local, remote));
                  return true;
                }} />
              </div>
            </Block>
          </div>
        </>
      )}

      {info && info.state !== "unreachable" && info.state !== "not-installed" && (
        <Block
          title="UPDATE"
          hint="BUILD MOLE ON THE NODE"
          right={update.running ? <Btn tone="bad" onClick={update.cancel}><X size={11} /> CANCEL</Btn> : undefined}
        >
          <div className="flex items-stretch gap-3">
            <div className="no-drag flex flex-1 items-center border border-line">
              <span className="px-3 text-[10px] tracking-[0.25em] text-faint">BRANCH</span>
              <input value={branch} onChange={(e) => setBranch(e.target.value)} onKeyDown={(e) => e.key === "Enter" && !update.running && runUpdate()} placeholder="latest main  ·  or feat/some-branch to try it without a release" className="min-w-0 flex-1 bg-transparent py-2.5 pr-3 text-xs text-foreground outline-none placeholder:text-faint select-text" />
            </div>
            <Btn tone="hot" disabled={update.running} onClick={runUpdate} className="min-w-44">
              <Download size={13} /> {update.running ? "UPDATING…" : branch.trim() ? "INSTALL BRANCH" : "UPDATE NODE"}
            </Btn>
          </div>
          {(update.text || update.running) && (
            <div className="mt-3">
              <Term text={update.text} running={update.running} />
              {!update.running && update.code === 0 && d?.running && <div className="mt-2 text-[11px] text-warn">Its daemon keeps running the old binary until you RESTART it.</div>}
            </div>
          )}
        </Block>
      )}

      {info && info.state !== "unreachable" && info.state !== "not-installed" && (
        <Block title="CONSOLE" hint={`mole -n ${name} …`} right={cons.text ? <button onClick={cons.clear} className="no-drag text-[10px] tracking-[0.2em] text-faint transition hover:text-foreground">CLEAR</button> : undefined}>
          <div className="mb-3 flex flex-wrap gap-1.5">
            {QUICK.map((q) => (
              <button key={q} disabled={cons.running} onClick={() => void runCmd(q)} className="no-drag border border-line px-2.5 py-1 text-[10px] tracking-[0.15em] text-dim transition enabled:hover:border-hot enabled:hover:text-hot disabled:opacity-30">{q}</button>
            ))}
          </div>
          <div className="no-drag flex items-stretch border border-line focus-within:border-hot">
            <span className="flex items-center pl-3 pr-2 text-xs text-hot">$ mole -n {name}</span>
            <input
              value={cmd}
              onChange={(e) => setCmd(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void runCmd(cmd);
                else if (e.key === "ArrowUp" && hist.current.length) {
                  e.preventDefault();
                  histAt.current = Math.min(histAt.current + 1, hist.current.length - 1);
                  setCmd(hist.current[histAt.current]);
                } else if (e.key === "ArrowDown") {
                  e.preventDefault();
                  histAt.current = Math.max(histAt.current - 1, -1);
                  setCmd(histAt.current < 0 ? "" : hist.current[histAt.current]);
                }
              }}
              placeholder="ports list"
              spellCheck={false}
              className="min-w-0 flex-1 bg-transparent py-2.5 text-xs text-foreground outline-none placeholder:text-faint select-text"
            />
            {cons.running ? (
              <button onClick={cons.cancel} className="flex items-center gap-1.5 border-l border-line px-4 text-[11px] tracking-[0.2em] text-bad transition hover:bg-bad hover:text-background"><Square size={11} /> STOP</button>
            ) : (
              <button disabled={!cmd.trim()} onClick={() => void runCmd(cmd)} className="flex items-center gap-1.5 border-l border-line px-4 text-[11px] tracking-[0.2em] text-hot transition enabled:hover:bg-hot enabled:hover:text-background disabled:opacity-30"><CornerDownLeft size={12} /> RUN</button>
            )}
          </div>
          <div className="mt-3">
            <Term text={cons.text} running={cons.running} empty="Runs mole on that machine, as if typed there. No terminal: commands that need one (pickers, editors) won't work; use a terminal for those." />
          </div>
        </Block>
      )}
    </div>
  );
}

/* ───────── the page ───────── */

const EMPTY_LIST: NodeList = { self: { hostname: "", os: "", arch: "" }, nodes: [], cliReady: true, cliError: "" };

export default function NodesPage({ onGoSetup }: { onGoSetup: () => void }) {
  const [list, setList] = useState<NodeList>(EMPTY_LIST);
  const [loaded, setLoaded] = useState(false);
  const [probes, setProbes] = useState<Probes>({});
  const [selected, setSelected] = useState<string | null>(null);
  const [form, setForm] = useState<{ edit?: NodeEntry } | null>(null);
  const [installing, setInstalling] = useState<string | null>(null);
  const [selfVersion, setSelfVersion] = useState("");
  const seq = useRef<Record<string, number>>({});
  const names = useRef<string[]>([]);

  // The registry, from the config: cheap, so it is re-read often.
  const loadList = useCallback(async () => {
    try {
      const l = await api.nodes();
      setList((old) => (JSON.stringify(old) === JSON.stringify(l) ? old : l));
    } catch (e) {
      toast.error(errMsg(e));
    } finally {
      setLoaded(true);
    }
  }, []);
  useEffect(() => {
    void loadList();
    const t = setInterval(loadList, 3000);
    return () => clearInterval(t);
  }, [loadList]);
  useEffect(() => {
    api.cli().then((c) => setSelfVersion(/v(\d+\.\d+\.\d+)/.exec(c.version ?? "")?.[1] ?? "")).catch(() => {});
  }, []);

  // Contacting a node opens an SSH connection, so each is probed on its own.
  const probe = useCallback(async (name: string) => {
    const n = (seq.current[name] = (seq.current[name] ?? 0) + 1);
    setProbes((p) => ({ ...p, [name]: { info: p[name]?.info ?? null, loading: true, at: p[name]?.at ?? 0 } }));
    let info: NodeInfo;
    try {
      info = await api.probe(name);
    } catch (e) {
      info = { name, target: "", sshPort: 0, bin: "", insecure: false, implicit: false, state: "unreachable", error: errMsg(e), version: "", build: "", os: "", arch: "", hostname: "", agent: 0, connectMs: 0, roundTripMs: 0, daemon: null, cliOutdated: false };
    }
    if (seq.current[name] !== n) return;
    setProbes((p) => ({ ...p, [name]: { info, loading: false, at: Date.now() } }));
  }, []);
  const probeAll = useCallback(() => { names.current.forEach((n) => void probe(n)); }, [probe]);

  const key = list.nodes.map((n) => `${n.name}|${n.target}|${n.sshPort}|${n.bin}|${n.insecure}`).join("\n");
  useEffect(() => {
    names.current = list.nodes.map((n) => n.name);
    if (!list.cliReady) return;
    for (const n of list.nodes) if (!probes[n.name] && !seq.current[n.name]) void probe(n.name);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, list.cliReady]);
  useEffect(() => {
    const t = setInterval(() => { if (document.visibilityState === "visible") probeAll(); }, 20_000);
    return () => clearInterval(t);
  }, [probeAll]);

  // A node edited in place is a different node: ask again.
  const lastKey = useRef(key);
  useEffect(() => {
    if (lastKey.current === key) return;
    const old = new Map(lastKey.current.split("\n").filter(Boolean).map((l) => [l.split("|")[0], l]));
    lastKey.current = key;
    for (const l of key.split("\n").filter(Boolean)) {
      const nm = l.split("|")[0];
      if (old.has(nm) && old.get(nm) !== l) void probe(nm);
    }
  }, [key, probe]);

  const counts = useMemo(() => {
    const c = { ready: 0, noAgent: 0, noMole: 0, down: 0, checking: 0 };
    for (const n of list.nodes) {
      const v = viewOf(probes[n.name]);
      if (v === "ready") c.ready++; else if (v === "no-agent") c.noAgent++; else if (v === "not-installed") c.noMole++; else if (v === "unreachable") c.down++; else c.checking++;
    }
    return c;
  }, [list.nodes, probes]);
  const lastCheck = Math.max(0, ...Object.values(probes).map((p) => p?.at ?? 0));
  const [, tick] = useState(0);
  useEffect(() => { const t = setInterval(() => tick((x) => x + 1), 1000); return () => clearInterval(t); }, []);
  const ago = lastCheck ? Math.max(0, Math.round((Date.now() - lastCheck) / 1000)) : -1;

  const select = (name: string) => setSelected((s) => (s === name ? null : name));
  const openForm = (edit?: NodeEntry) => { setForm({ edit }); };
  const existing = list.nodes.map((n) => n.name);

  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      {!list.cliReady && loaded && (
        <div className="flex items-center gap-4 border-b border-warn bg-warn/10 px-5 py-3">
          <Plug size={16} className="text-warn" />
          <div className="flex-1 text-xs text-warn">{list.cliError || "The mole CLI could not be used."}</div>
          <Btn tone="warn" onClick={onGoSetup}>OPEN SETUP</Btn>
        </div>
      )}

      <MeshMap self={list.self} nodes={list.nodes} probes={probes} selected={selected} onSelect={select} onAdd={() => openForm()} />

      {/* toolbar */}
      <div className="no-drag flex items-center gap-5 border-b border-line px-5 py-2.5 text-[10px] tracking-[0.25em]">
        <span className="font-bold text-foreground">NODES</span>
        <span className="text-dim tabular-nums">{list.nodes.length}</span>
        {list.nodes.length > 0 && (
          <span className="flex items-center gap-4 text-faint">
            {counts.ready > 0 && <span className="text-live">{counts.ready} READY</span>}
            {counts.noAgent > 0 && <span className="text-warn">{counts.noAgent} NO AGENT</span>}
            {counts.noMole > 0 && <span className="text-warn">{counts.noMole} NEED MOLE</span>}
            {counts.down > 0 && <span className="text-bad">{counts.down} DOWN</span>}
            {counts.checking > 0 && <span>{counts.checking} CHECKING…</span>}
          </span>
        )}
        <span className="ml-auto text-faint">{ago >= 0 ? `CHECKED ${ago < 5 ? "JUST NOW" : `${ago}S AGO`}` : ""}</span>
        <Btn disabled={!list.nodes.length || !list.cliReady} onClick={probeAll}><RotateCw size={12} /> REFRESH</Btn>
        <Btn tone="hot" disabled={!list.cliReady} onClick={() => openForm()}><Plus size={13} /> ADD NODE</Btn>
      </div>

      {form && !form.edit && (
        <NodeForm
          key="new"
          existing={existing}
          onCancel={() => setForm(null)}
          onDone={(name, install) => { setForm(null); if (install) setInstalling(name); void loadList().then(() => { setSelected(name); if (!install) void probe(name); }); }}
        />
      )}

      {/* rows */}
      {list.nodes.map((n, i) => {
        const pr = probes[n.name];
        const v = viewOf(pr);
        const info = pr?.info;
        const open = selected === n.name;
        const editing = form?.edit?.name === n.name;
        const older = !!info?.version && !!selfVersion && olderThan(info.version, selfVersion);
        return (
          <div key={n.name}>
            <button onClick={() => select(n.name)} style={{ animationDelay: `${i * 35}ms` }} className={`no-drag rise flex w-full items-center gap-4 border-b border-line px-5 py-3.5 text-left transition hover:bg-white/[0.03] ${open ? "bg-white/[0.04]" : ""}`}>
              <span className={`size-2.5 shrink-0 ${TONE[v].bg} ${v === "checking" || pr?.loading ? "blink" : ""}`} />
              <span className="w-40 shrink-0 truncate font-display text-2xl font-bold">{n.name}</span>
              <span className="w-56 shrink-0 truncate text-xs text-dim">{n.target}{n.implicit && <span className="ml-2 text-[9px] tracking-[0.2em] text-faint">CONFIG</span>}</span>
              <span className={`w-28 shrink-0 text-[10px] tracking-[0.25em] ${TONE[v].text}`}>{TONE[v].label}</span>
              <span className="flex min-w-0 flex-1 items-center gap-2 overflow-hidden text-[10px] tracking-[0.15em]">
                {info && info.state !== "unreachable" && (
                  <>
                    {info.os && <span className="whitespace-nowrap border border-line px-2 py-0.5 text-dim">{info.os.toUpperCase()}/{info.arch.toUpperCase()}</span>}
                    {info.version && <span className={`whitespace-nowrap border px-2 py-0.5 ${older ? "border-warn text-warn" : "border-line text-dim"}`}>V{info.version}{older ? " · OLDER" : ""}</span>}
                    {info.state === "ready" && info.daemon && (
                      <span className={`whitespace-nowrap border px-2 py-0.5 ${info.daemon.running ? "border-live/50 text-live" : "border-line text-faint"}`}>
                        DAEMON {info.daemon.running ? `UP${info.daemon.uptime ? ` ${info.daemon.uptime}` : ""}` : "DOWN"}
                        {info.daemon.running && ` · ${info.daemon.ports.length}↓ ${info.daemon.push.length}↑`}
                      </span>
                    )}
                  </>
                )}
                {info?.state === "unreachable" && <span className="truncate text-bad/80">{info.error}</span>}
              </span>
              <span className="w-16 shrink-0 text-right text-[11px] tabular-nums text-dim">{v === "ready" ? fmtMs(info!.connectMs) : ""}</span>
              <ChevronDown size={15} className={`shrink-0 text-faint transition ${open ? "rotate-180 text-hot" : ""}`} />
            </button>
            {editing && (
              <NodeForm key={`edit-${n.name}`} initial={n} existing={existing} onCancel={() => setForm(null)} onDone={() => { setForm(null); void loadList().then(() => probe(n.name)); }} />
            )}
            {open && !editing && (
              <NodeDetail
                key={n.name}
                entry={n}
                probe={pr}
                selfVersion={selfVersion}
                reprobe={() => probe(n.name)}
                onEdit={() => openForm(n)}
                autoInstall={installing === n.name}
                onAutoInstalled={() => setInstalling(null)}
                onRemoved={() => { setSelected(null); setProbes((p) => { const { [n.name]: _gone, ...rest } = p; return rest; }); void loadList(); }}
              />
            )}
          </div>
        );
      })}

      {loaded && list.nodes.length === 0 && (
        <div className="px-5 py-10">
          <div className="font-display text-3xl font-bold">NO NODES YET</div>
          <div className="mt-3 max-w-xl text-xs leading-5 text-dim">
            A node is another machine you can reach over SSH that runs mole. Add one and you can manage it from here: its ports, its daemon, its updates, even a branch you want to try, without opening a terminal on it.
          </div>
          <div className="mt-4"><Btn tone="hot" disabled={!list.cliReady} onClick={() => openForm()}><Plus size={13} /> ADD YOUR FIRST NODE</Btn></div>
        </div>
      )}
    </div>
  );
}
