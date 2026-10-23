import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { Activity, Cable, Terminal, Plus, RotateCw, ScrollText, Square, Play, X, ArrowLeft, ArrowRight } from "lucide-react";
import type { Status, CLIStatus, Event as Conn } from "../bindings/github.com/Luqueee/mole/desktop";
import type { PushInfo } from "../bindings/github.com/Luqueee/mole/internal/admin";
import { api, MOCK } from "./api";

type Tab = "status" | "ports" | "logs" | "setup";
const NAV: { id: Tab; label: string; icon: typeof Activity }[] = [
  { id: "status", label: "STATUS", icon: Activity },
  { id: "ports", label: "PORTS", icon: Cable },
  { id: "logs", label: "LOGS", icon: ScrollText },
  { id: "setup", label: "SETUP", icon: Terminal },
];
const LEVEL = { ERROR: "text-bad", WARN: "text-warn", INFO: "text-live", DEBUG: "text-dim" } as const;
function parseLog(l: string) {
  const t = /time=(\S+)/.exec(l)?.[1];
  const lv = (/level=(\w+)/.exec(l)?.[1] ?? "INFO") as keyof typeof LEVEL;
  const msg = l.replace(/time=\S+\s*/, "").replace(/level=\w+\s*/, "");
  return { time: t ? t.slice(11, 19) : "", lv, msg };
}
function LogMsg({ text }: { text: string }) {
  const m = /^msg=(?:"((?:[^"\\]|\\.)*)"|(\S+))\s*(.*)$/.exec(text);
  const head = m ? (m[1] ?? m[2]) : text;
  const rest = m ? m[3] : "";
  const kv = [...rest.matchAll(/(\w+)=("(?:[^"\\]|\\.)*"|\S+)/g)];
  return (
    <>
      <span className="text-foreground">{head}</span>
      {kv.map((x, i) => (
        <span key={i} className="ml-3">
          <span className="text-faint">{x[1]}=</span>
          <span className={x[1] === "err" ? "text-bad" : "text-hot"}>{x[2].replace(/^"|"$/g, "")}</span>
        </span>
      ))}
    </>
  );
}
const errMsg = (e: unknown) => (e instanceof Error ? e.message : String(e));

/* ───────── small pieces ───────── */

function Spark({ data, color = "var(--accent-hot)" }: { data: number[]; color?: string }) {
  const w = 160, h = 36;
  const max = Math.max(...data, 1);
  const pts = data.map((v, i) => `${(i / Math.max(data.length - 1, 1)) * w},${h - (v / max) * (h - 4) - 2}`);
  return (
    <svg viewBox={`0 0 ${w} ${h}`} className="h-7 w-full" preserveAspectRatio="none">
      <polyline points={pts.join(" ")} fill="none" stroke={color} strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
      <polyline points={`0,${h} ${pts.join(" ")} ${w},${h}`} fill={color} opacity=".12" />
    </svg>
  );
}

function Cell({ label, value, hist, tone, caption, size = "text-4xl" }: { label: string; value: string | number; hist?: number[]; tone?: string; caption?: string; size?: string }) {
  return (
    <div className="rise relative flex flex-col justify-between gap-2 border-r border-line p-4 last:border-r-0">
      <div className="flex items-center justify-between text-[10px] tracking-[0.25em] text-dim">
        {label}
        <span className="size-1.5" style={{ background: tone ?? "var(--accent-hot)" }} />
      </div>
      <div className={`font-display ${size} whitespace-nowrap font-bold leading-none tabular-nums`} style={{ color: tone }}>
        {value}
      </div>
      {hist ? <Spark data={hist} color={tone} /> : <div className="h-7 text-[10px] leading-7 tracking-[0.25em] text-faint">{caption}</div>}
    </div>
  );
}

const TRIP = 2000; // ms for a full round trip along the trunk
type Trip = { id: number; dir: string; port: number; failed: boolean; row: number };
type Seen = Conn & { at: number };
const fmtBytes = (n: number) => (n < 1024 ? `${n}B` : n < 1048576 ? `${(n / 1024).toFixed(1)}KB` : `${(n / 1048576).toFixed(1)}MB`);
const hhmmss = (ms: number) => new Date(ms).toTimeString().slice(0, 8);

type Lane = { dir: string; port: number };

/** Lanes (direction + port) ordered by recent activity first, then numerically. */
function rankPorts(lanes: Lane[], conns: Seen[], now: number) {
  const key = (dir: string, port: number) => `${dir}:${port}`;
  const last = new Map<string, number>();
  const count = new Map<string, number>();
  for (const c of conns) {
    const k = key(c.dir, c.port);
    last.set(k, Math.max(last.get(k) ?? 0, c.at));
    if (now - c.at < 60_000) count.set(k, (count.get(k) ?? 0) + 1);
  }
  return [...lanes]
    .sort((x, y) => (last.get(key(y.dir, y.port)) ?? 0) - (last.get(key(x.dir, x.port)) ?? 0) || x.port - y.port)
    .map((l) => {
      const k = key(l.dir, l.port);
      return { ...l, hot: now - (last.get(k) ?? 0) < 3000, n: count.get(k) ?? 0, seen: last.has(k) };
    });
}

function LocalBox({ lanes, conns }: { lanes: Lane[]; conns: Seen[] }) {
  const ranked = rankPorts(lanes, conns, Date.now());
  const shown = ranked.slice(0, 6);
  return (
    <div className="flex min-h-0 flex-col border border-line bg-panel p-4">
      <div className="text-[10px] tracking-[0.3em] text-dim">LOCAL</div>
      <div className="mt-1 font-display text-xl font-bold">127.0.0.1</div>
      <div className="mt-3 min-h-0 flex-1 space-y-1 overflow-hidden text-[11px]">
        {shown.map((r) => (
          <div key={`${r.dir}:${r.port}`} className="flex items-center gap-2">
            <span className={`size-1.5 ${r.hot ? "bg-hot shadow-[0_0_8px_var(--accent-hot)]" : r.seen ? "bg-live" : "bg-faint"}`} />
            <span className={r.seen ? "text-foreground" : "text-dim"}>{r.dir === "push" ? "←" : ""}:{r.port}</span>
            <span className="ml-auto tabular-nums text-faint">{r.n ? `×${r.n}` : ""}</span>
          </div>
        ))}
      </div>
      {ranked.length > shown.length && <div className="mt-2 text-[10px] tracking-[0.25em] text-faint">+{ranked.length - shown.length} IDLE</div>}
    </div>
  );
}

function RemoteBox({ remote, ports, s }: { remote: string; ports: number; s: Status }) {
  const rows: [string, string][] = [
    ["TUNNEL", s.running ? "UP" : "DOWN"],
    ["FORWARDED", String(ports)],
    ["PUSHED", String((s.push ?? []).filter((p) => p.state === "active").length)],
    ["CONNS", String(s.stats.active_conns)],
    ["DIAL FAIL", String(s.stats.failed_dials)],
  ];
  return (
    <div className="flex min-h-0 flex-col border border-line bg-panel p-4">
      <div className="text-[10px] tracking-[0.3em] text-dim">REMOTE</div>
      <div className="mt-1 break-all font-display text-xl font-bold text-hot">{remote || "—"}</div>
      <div className="mt-3 space-y-1 text-[11px]">
        {rows.map(([k, v]) => (
          <div key={k} className="flex justify-between">
            <span className="text-faint">{k}</span>
            <span className={k === "TUNNEL" ? (s.running ? "text-live" : "text-bad") : "tabular-nums"}>{v}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function Trunk({ running, trips, last }: { running: boolean; trips: Trip[]; last: Seen | null }) {
  return (
    <div className="relative flex items-center">
      {/* the pipe */}
      <div className={`absolute inset-x-0 top-1/2 h-[52px] -translate-y-1/2 border-y ${running ? "border-live/40" : "border-faint/40"} bg-white/[0.02]`} />
      <div className={`absolute inset-x-0 top-1/2 h-px -translate-y-1/2 ${running ? "lane" : "border-t border-dashed border-faint/50"}`} />
      {/* packets */}
      <div className="absolute inset-x-0 top-1/2 h-[52px] -translate-y-1/2">
        {trips.map((t) => (
          <span key={t.id} className={`pill ${t.dir === "push" ? "pill-push" : ""} ${t.failed ? "pill-bad" : ""}`} style={{ top: 4 + t.row * 15, animationDuration: `${TRIP}ms` }}>
            {t.dir === "push" ? "←" : ""}:{t.port}
          </span>
        ))}
      </div>
      {/* MOLE sits on top of the pipe: packets pass "through" it */}
      <div className="relative z-10 mx-auto flex h-full w-28 flex-col items-center justify-center bg-hot text-background">
        <div className="font-display text-xl font-extrabold tracking-[0.25em]">MOLE</div>
        <div className="mt-1 text-[9px] tracking-[0.3em]">1× SSH</div>
        <div className="absolute inset-x-0 bottom-3 px-1 text-center text-[9px] leading-4 tracking-[0.12em]">
          {last ? `${last.dir === "push" ? "←" : "→"}:${last.port} ${last.failed ? "FAILED" : "· " + fmtBytes(last.bytesIn + last.bytesOut)}` : "IDLE"}
        </div>
      </div>
    </div>
  );
}

function Tunnel({ s, trips, conns, supported }: { s: Status; trips: Trip[]; conns: Seen[]; supported: boolean }) {
  const ports = s.ports ?? [];
  const lanes = [
    ...ports.map((port) => ({ dir: "pull", port })),
    ...(s.push ?? []).map((p) => ({ dir: "push", port: p.local })),
  ];
  return (
    <div className="grid-bg relative shrink-0 border-b border-line p-4">
      <div className="grid h-[176px] grid-cols-[210px_1fr_210px] gap-0">
        <LocalBox lanes={lanes} conns={conns} />
        <Trunk running={s.running} trips={trips} last={conns[conns.length - 1] ?? null} />
        <RemoteBox remote={s.remote} ports={ports.length} s={s} />
      </div>
      {s.running && !supported && (
        <div className="mt-3 text-center text-[9px] tracking-[0.25em] text-warn">DAEMON OUTDATED · RESTART FOR LIVE TRAFFIC</div>
      )}
    </div>
  );
}

function Waterfall({ conns }: { conns: Seen[] }) {
  const rows = [...conns].reverse().slice(0, 60);
  const max = Math.max(...rows.map((r) => r.durMs), 50);
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex shrink-0 items-center gap-4 border-b border-line px-5 py-2 text-[10px] tracking-[0.25em] text-dim">
        <span className="w-16">TIME</span>
        <span className="w-16">DIR</span>
        <span className="w-16">PORT</span>
        <span className="flex-1">DURATION</span>
        <span className="w-28 text-right">↑ OUT</span>
        <span className="w-28 text-right">↓ IN</span>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain">
        {rows.map((r) => (
          <div key={r.id} className="rise flex items-center gap-4 border-b border-line/60 px-5 py-[7px] text-[11px] hover:bg-white/[0.03]">
            <span className="w-16 text-faint">{hhmmss(r.at)}</span>
            <span className={`w-16 whitespace-nowrap text-[9px] font-bold tracking-[0.15em] ${r.dir === "push" ? "text-warn" : "text-hot"}`}>{r.dir === "push" ? "← PUSH" : "PULL →"}</span>
            <span className="w-16 font-bold">:{r.port}</span>
            <span className="flex flex-1 items-center gap-3">
              <span className="h-2 bg-line" style={{ width: "100%", maxWidth: "70%" }}>
                <span className={`block h-full ${r.failed ? "bg-bad" : "bg-hot"}`} style={{ width: `${Math.max((r.durMs / max) * 100, 2)}%` }} />
              </span>
              <span className={`w-16 tabular-nums ${r.failed ? "text-bad" : ""}`}>{r.failed ? "FAILED" : `${r.durMs}ms`}</span>
            </span>
            <span className="w-28 text-right tabular-nums text-dim">{fmtBytes(r.bytesOut)}</span>
            <span className="w-28 text-right tabular-nums text-live">{fmtBytes(r.bytesIn)}</span>
          </div>
        ))}
        {rows.length === 0 && <div className="p-6 text-[11px] tracking-[0.3em] text-faint">WAITING FOR TRAFFIC — HIT A FORWARDED PORT</div>}
      </div>
    </div>
  );
}

/* ───────── pages ───────── */

function StatusPage({ s, hist, trips, conns, supported }: { s: Status; hist: Record<string, number[]>; trips: Trip[]; conns: Seen[]; supported: boolean }) {
  const m = /^(?:(\d+)h)?(?:(\d+)m)?/.exec(s.stats.uptime || "");
  const up = s.running ? `${m?.[1] ?? 0}h ${m?.[2] ?? 0}m` : "—";
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <Tunnel s={s} trips={trips} conns={conns} supported={supported} />
      <div className="grid shrink-0 grid-cols-4 border-b border-line">
        <Cell label="UPTIME" value={up} caption={s.running ? "SINCE LAST START" : "DAEMON DOWN"} />
        <Cell label="ACTIVE" value={s.stats.active_conns} hist={hist.active} tone="var(--live)" />
        <Cell label="TOTAL" value={s.stats.total_conns.toLocaleString("en")} hist={hist.total} />
        <Cell label="FAILED" value={s.stats.failed_dials.toLocaleString("en")} hist={hist.failed} tone="var(--bad)" />
      </div>
      <Waterfall conns={conns} />
    </div>
  );
}

type Run = (l: string, f: () => Promise<unknown>) => Promise<void>;
const validPort = (p: number) => Number.isInteger(p) && p >= 1 && p <= 65535;

function SectionHead({ title, hint }: { title: string; hint: string }) {
  return (
    <div className="flex items-baseline gap-4 border-b border-line bg-white/[0.02] px-5 py-2 text-[10px] tracking-[0.25em]">
      <span className="font-bold text-foreground">{title}</span>
      <span className="text-faint">{hint}</span>
    </div>
  );
}

function AddRow({ value, onChange, onAdd, placeholder, pattern }: { value: string; onChange: (v: string) => void; onAdd: () => void; placeholder: string; pattern: RegExp }) {
  return (
    <div className="no-drag flex items-stretch border-b border-line">
      <div className="flex items-center px-5 text-hot"><Plus size={20} /></div>
      <input
        value={value}
        onChange={(e) => onChange(e.target.value.replace(pattern, ""))}
        onKeyDown={(e) => e.key === "Enter" && onAdd()}
        placeholder={placeholder}
        className="flex-1 bg-transparent py-5 font-display text-3xl font-bold tracking-wide text-foreground outline-none placeholder:text-faint select-text"
      />
      <button onClick={onAdd} className="bg-hot px-8 font-display text-sm font-bold tracking-[0.2em] text-background transition hover:brightness-110 active:translate-y-px">
        ADD
      </button>
    </div>
  );
}

function PortsPage({ discover, forwarded, push, run }: { discover: number[]; forwarded: number[]; push: PushInfo[]; run: Run }) {
  const [v, setV] = useState("");
  const [pv, setPv] = useState("");
  const add = () => {
    const p = Number(v);
    if (!validPort(p)) return toast.error("Port must be 1–65535");
    run(`+ ${p}`, () => api.add(p)).then(() => setV(""));
  };
  const addPush = () => {
    const m = /^(\d+)(?::(\d+))?$/.exec(pv);
    const local = Number(m?.[1]);
    const remote = m?.[2] ? Number(m[2]) : 0;
    if (!m || !validPort(local) || (remote !== 0 && !validPort(remote))) return toast.error("Use LOCAL or LOCAL:REMOTE, ports 1–65535");
    run(`← ${local}${remote ? `:${remote}` : ""}`, () => api.pushAdd(local, remote)).then(() => setPv(""));
  };
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <SectionHead title="PULL" hint="REMOTE PORTS APPEAR ON THIS MACHINE (-L)" />
      <AddRow value={v} onChange={setV} onAdd={add} placeholder="ADD DISCOVERY PORT" pattern={/\D/g} />
      {discover.map((p, i) => {
        const live = forwarded.includes(p);
        return (
          <div key={p} style={{ animationDelay: `${i * 30}ms` }} className="rise group flex items-center border-b border-line px-5 py-4 transition hover:bg-white/[0.03]">
            <span className={`mr-4 size-2 ${live ? "bg-live" : "bg-faint"}`} />
            <span className="w-28 font-display text-3xl font-bold tabular-nums">{p}</span>
            <span className="flex flex-1 items-center gap-3 text-xs text-dim">
              localhost:{p} <ArrowLeft size={14} className="text-hot" /> remote:{p}
            </span>
            <span className={`mr-6 text-[10px] tracking-[0.25em] ${live ? "text-live" : "text-faint"}`}>{live ? "FORWARDING" : "IDLE"}</span>
            <button aria-label={`Remove ${p}`} onClick={() => run(`− ${p}`, () => api.remove(p))} className="no-drag p-1 text-faint transition hover:text-bad">
              <X size={18} />
            </button>
          </div>
        );
      })}
      {discover.length === 0 && <div className="p-8 text-xs tracking-[0.3em] text-faint">NO CANDIDATES</div>}

      <SectionHead title="PUSH" hint="LOCAL PORTS APPEAR ON THE REMOTE (-R)" />
      <AddRow value={pv} onChange={setPv} onAdd={addPush} placeholder="PUSH LOCAL[:REMOTE]" pattern={/[^\d:]/g} />
      {push.map((p, i) => (
        <div key={p.local} style={{ animationDelay: `${i * 30}ms` }} className="rise group flex items-center border-b border-line px-5 py-4 transition hover:bg-white/[0.03]">
          <span className={`mr-4 size-2 ${p.state === "active" ? "bg-warn" : p.state === "failed" ? "bg-bad" : "bg-faint"}`} />
          <span className="w-28 font-display text-3xl font-bold tabular-nums">{p.local}</span>
          <span className="flex flex-1 items-center gap-3 text-xs text-dim">
            localhost:{p.local} <ArrowRight size={14} className="text-warn" /> remote:{p.remote}
            {p.state === "failed" && p.error && <span className="text-bad">· {p.error}</span>}
          </span>
          <span className={`mr-6 text-[10px] tracking-[0.25em] ${p.state === "active" ? "text-warn" : p.state === "failed" ? "text-bad" : "text-faint"}`}>
            {p.state === "active" ? "PUSHING" : p.state === "failed" ? "FAILED" : "IDLE"}
          </span>
          <button aria-label={`Remove push ${p.local}`} onClick={() => run(`− ${p.local}`, () => api.pushRemove(p.local))} className="no-drag p-1 text-faint transition hover:text-bad">
            <X size={18} />
          </button>
        </div>
      ))}
      {push.length === 0 && <div className="p-8 text-xs tracking-[0.3em] text-faint">NOTHING PUSHED</div>}
    </div>
  );
}


function SetupPage({ run }: { run: Run }) {
  const [cli, setCli] = useState<CLIStatus | null>(null);
  const load = useCallback(() => api.cli().then(setCli).catch(() => {}), []);
  useEffect(() => void load(), [load]);
  const state = !cli ? "…" : !cli.installed ? "NOT INSTALLED" : cli.bundled && !cli.upToDate ? "UPDATE AVAILABLE" : "INSTALLED";
  const tone = state === "INSTALLED" ? "text-live" : state === "UPDATE AVAILABLE" ? "text-warn" : "text-faint";
  const label = !cli?.installed ? "INSTALL" : cli.upToDate ? "REINSTALL" : "UPDATE";
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <SectionHead title="CLI" hint="THE mole COMMAND FOR YOUR TERMINAL" />
      <div className="flex items-center border-b border-line px-5 py-5">
        <span className={`mr-4 size-2 ${state === "INSTALLED" ? "bg-live" : state === "UPDATE AVAILABLE" ? "bg-warn" : "bg-faint"}`} />
        <div className="flex-1">
          <div className={`font-display text-3xl font-bold tracking-wide ${tone}`}>{state}</div>
          <div className="mt-2 text-xs text-dim select-text">
            {cli?.installed ? `${cli.path}${cli.version ? ` · ${cli.version}` : ""}` : `will be installed to ${cli?.installDir ?? "…"}`}
          </div>
        </div>
        <button
          disabled={!cli?.bundled}
          onClick={() => run("CLI installed", api.cliInstall).then(load)}
          className="no-drag bg-hot px-8 py-4 font-display text-sm font-bold tracking-[0.2em] text-background transition enabled:hover:brightness-110 enabled:active:translate-y-px disabled:opacity-30"
        >
          {label}
        </button>
      </div>
      {cli && !cli.bundled && <div className="border-b border-line px-5 py-4 text-xs text-dim">This build does not embed the CLI (run <span className="text-foreground">wails3 task bundle:cli</span> before building).</div>}
      {cli?.installed && !cli.dirOnPath && cli.path.startsWith(cli.installDir) && (
        <div className="border-b border-line px-5 py-4 text-xs text-warn select-text">{cli.installDir} is not in your PATH: add it to use `mole` in a terminal.</div>
      )}
      {cli?.installed && <div className="px-5 py-4 text-[11px] text-faint">A running daemon keeps its old binary until you press RESTART.</div>}
    </div>
  );
}

function LogsPage({ lines }: { lines: string[] }) {
  const [filter, setFilter] = useState<"ALL" | keyof typeof LEVEL>("ALL");
  const ref = useRef<HTMLDivElement>(null);
  const parsed = useMemo(() => lines.map(parseLog), [lines]);
  const shown = parsed.filter((p) => filter === "ALL" || p.lv === filter);
  const primed = useRef(false);
  const [atBottom, setAtBottom] = useState(true);
  const toBottom = () => { const el = ref.current; if (el) el.scrollTo({ top: el.scrollHeight, behavior: "smooth" }); };
  useEffect(() => {
    const el = ref.current;
    if (!el || !shown.length) return;
    // Start at the newest line, then only follow while the user is near the bottom.
    if (!primed.current || el.scrollHeight - el.scrollTop - el.clientHeight < 120) el.scrollTop = el.scrollHeight;
    primed.current = true;
  }, [shown.length]);
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="no-drag flex border-b border-line text-[10px] tracking-[0.25em]">
        {(["ALL", "ERROR", "WARN", "INFO", "DEBUG"] as const).map((f) => (
          <button key={f} onClick={() => setFilter(f)} className={`border-r border-line px-5 py-3 transition ${filter === f ? "bg-foreground text-background" : "text-dim hover:text-foreground"}`}>
            {f}
          </button>
        ))}
        <div className="ml-auto flex items-center px-5 text-faint">{shown.length} LINES</div>
      </div>
      <div className="relative min-h-0 flex-1">
      <div
        ref={ref}
        onScroll={(e) => { const el = e.currentTarget; setAtBottom(el.scrollHeight - el.scrollTop - el.clientHeight < 60); }}
        className="absolute inset-0 overflow-y-auto overscroll-contain p-4 text-[12px] leading-6 select-text"
      >
        {shown.map((p, i) => (
          <div key={i} className="flex gap-3 hover:bg-white/[0.03]">
            <span className="w-9 text-right text-faint">{i + 1}</span>
            <span className="text-faint">{p.time}</span>
            <span className={`w-12 font-bold ${LEVEL[p.lv] ?? "text-dim"}`}>{p.lv}</span>
            <span className="min-w-0 flex-1 break-words"><LogMsg text={p.msg} /></span>
          </div>
        ))}
      </div>
      {!atBottom && (
        <button onClick={toBottom} className="no-drag absolute bottom-4 right-6 bg-hot px-4 py-2 text-[10px] font-bold tracking-[0.25em] text-background transition hover:brightness-110">
          ↓ LATEST
        </button>
      )}
      </div>
    </div>
  );
}

/* ───────── shell ───────── */

const EMPTY: Status = { running: false, adminAddr: "", remote: "", stats: { uptime: "", active_conns: 0, total_conns: 0, failed_dials: 0 }, ports: [], info: {}, error: "" } as unknown as Status;

export default function App() {
  const [tab, setTab] = useState<Tab>(() => (new URLSearchParams(location.search).get("tab") as Tab) || "status");
  const [s, setS] = useState<Status>(EMPTY);
  const [discover, setDiscover] = useState<number[]>([]);
  const [logs, setLogs] = useState<string[]>([]);
  const [hist, setHist] = useState<Record<string, number[]>>({ up: [], active: [], total: [], failed: [] });

  const lastTotal = useRef(0);
  const [trips, setTrips] = useState<Trip[]>([]);
  const [conns, setConns] = useState<Seen[]>([]);
  const rowSeq = useRef(0);
  const [supported, setSupported] = useState(true);
  const cursor = useRef<number | null>(null);
  const refresh = useCallback(async () => {
    try {
      const st = await api.status();
      setS(st);
      setDiscover(await api.discover());
      setLogs(await api.logs(400));
      const push = (a: number[], v: number) => [...a, v].slice(-40);
      const prev = lastTotal.current || st.stats.total_conns;
      lastTotal.current = st.stats.total_conns;
      setHist((h) => ({
        up: push(h.up, h.up.length),
        active: push(h.active, st.stats.active_conns),
        total: push(h.total, h.total.length ? Math.max(st.stats.total_conns - prev, 0) : 0),
        failed: push(h.failed, st.stats.failed_dials),
      }));
    } catch (e) {
      toast.error(errMsg(e));
    }
  }, []);

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 2000);
    return () => clearInterval(t);
  }, [refresh]);

  // Live traffic: poll the daemon's event ring and play each finished
  // connection as a round trip along its lane.
  useEffect(() => {
    let stop = false;
    const tick = async () => {
      try {
        const r = await api.events(cursor.current ?? 0);
        if (stop) return;
        setSupported(r.supported);
        const first = cursor.current === null;
        cursor.current = r.last;
        if (first || !r.events?.length) return; // don't replay history on startup
        const fresh = r.events.slice(-8);
        const at = Date.now();
        setConns((c) => [...c, ...fresh.map((e) => ({ ...e, at }))].slice(-120));
        setTrips((t) => [...t, ...fresh.map((e) => ({ id: e.id, dir: e.dir, port: e.port, failed: e.failed, row: rowSeq.current++ % 3 }))]);
        setTimeout(() => setTrips((t) => t.filter((x) => !fresh.some((f) => f.id === x.id))), TRIP + 200);
      } catch { /* daemon down: ignore */ }
    };
    tick();
    const t = setInterval(tick, 500);
    return () => { stop = true; clearInterval(t); };
  }, []);

  const run = async (label: string, fn: () => Promise<unknown>) => {
    try {
      await fn();
      toast.success(label);
      refresh();
    } catch (e) {
      toast.error(errMsg(e));
    }
  };

  return (
    <div className="noise scan flex h-screen flex-col bg-background text-foreground">
      {/* title bar */}
      <header className="drag flex h-14 shrink-0 items-center border-b border-line pl-24 pr-5">
        <div className="font-display text-xl font-bold tracking-[0.3em]">MOLE<span className="text-hot">/</span></div>
        <div className={`ml-5 flex items-center gap-2 border px-3 py-1 text-[10px] tracking-[0.25em] ${s.running ? "border-live text-live" : "border-faint text-dim"}`}>
          <span className={`size-1.5 ${s.running ? "blink bg-live" : "bg-faint"}`} />
          {s.running ? "LIVE" : "OFFLINE"}
        </div>
        {MOCK && <span className="ml-3 text-[10px] tracking-[0.25em] text-warn">MOCK</span>}
        <div className="no-drag ml-auto flex">
          <button disabled={s.running} onClick={() => run("started", api.start)} className="flex items-center gap-2 border border-line px-4 py-2 text-[11px] tracking-[0.2em] transition enabled:hover:bg-live enabled:hover:text-background disabled:opacity-30">
            <Play size={13} /> START
          </button>
          <button onClick={() => run("restarted", api.restart)} className="-ml-px flex items-center gap-2 border border-line px-4 py-2 text-[11px] tracking-[0.2em] transition hover:bg-foreground hover:text-background">
            <RotateCw size={13} /> RESTART
          </button>
          <button disabled={!s.running} onClick={() => run("stopped", api.stop)} className="-ml-px flex items-center gap-2 border border-line px-4 py-2 text-[11px] tracking-[0.2em] text-bad transition enabled:hover:bg-bad enabled:hover:text-background disabled:opacity-30">
            <Square size={13} /> STOP
          </button>
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        {/* rail */}
        <nav className="no-drag flex w-44 shrink-0 flex-col border-r border-line">
          {NAV.map((n, i) => (
            <button key={n.id} onClick={() => setTab(n.id)} className={`flex items-center gap-3 border-b border-line px-5 py-5 text-left text-xs tracking-[0.25em] transition ${tab === n.id ? "bg-hot text-background" : "text-dim hover:bg-white/[0.04] hover:text-foreground"}`}>
              <span className="text-[10px] opacity-60">0{i + 1}</span>
              <n.icon size={15} />
              {n.label}
            </button>
          ))}
          <div className="mt-auto border-t border-line p-5 text-[10px] leading-5 tracking-[0.2em] text-faint">
            {s.adminAddr || "NO ADMIN ADDR"}
          </div>
        </nav>

        <main className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
          {s.error && <div className="border-b border-bad bg-bad/10 px-5 py-2 text-[11px] text-bad">{s.error}</div>}
          {tab === "status" && <StatusPage s={s} hist={hist} trips={trips} conns={conns} supported={supported} />}
          {tab === "ports" && <PortsPage discover={discover} forwarded={s.ports ?? []} push={s.push ?? []} run={run} />}
          {tab === "logs" && <LogsPage lines={logs} />}
          {tab === "setup" && <SetupPage run={run} />}
        </main>
      </div>
      <Toaster position="bottom-right" />
    </div>
  );
}
