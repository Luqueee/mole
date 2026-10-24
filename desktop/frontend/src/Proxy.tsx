import { useCallback, useEffect, useState } from "react";
import { toast } from "sonner";
import { ArrowRight, ExternalLink, Plus, X } from "lucide-react";
import { api, type DomainInfo } from "./api";

const errMsg = (e: unknown) => (e instanceof Error ? e.message : String(e));
const validDomain = (d: string) => /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/.test(d);

function SectionHead({ title, hint }: { title: string; hint: string }) {
  return (
    <div className="flex items-baseline gap-4 border-b border-line bg-white/[0.02] px-5 py-2 text-[10px] tracking-[0.25em]">
      <span className="font-bold text-foreground">{title}</span>
      <span className="text-faint">{hint}</span>
    </div>
  );
}

function InputRow({ value, onChange, onSubmit, placeholder, pattern, action, busy }: { value: string; onChange: (v: string) => void; onSubmit: () => void; placeholder: string; pattern: RegExp; action: string; busy?: boolean }) {
  return (
    <div className="no-drag flex items-stretch border-b border-line">
      <div className="flex items-center px-5 text-hot"><Plus size={20} /></div>
      <input
        value={value}
        onChange={(e) => onChange(e.target.value.toLowerCase().replace(pattern, ""))}
        onKeyDown={(e) => e.key === "Enter" && !busy && onSubmit()}
        placeholder={placeholder}
        spellCheck={false}
        className="flex-1 bg-transparent py-5 font-display text-3xl font-bold tracking-wide text-foreground outline-none placeholder:text-faint select-text"
      />
      <button disabled={busy} onClick={onSubmit} className="bg-hot px-8 font-display text-sm font-bold tracking-[0.2em] text-background transition enabled:hover:brightness-110 enabled:active:translate-y-px disabled:opacity-40">
        {action}
      </button>
    </div>
  );
}

// A canonical target is "host:port" (the primary remote) or "@node/host:port".
const node = (t: string) => (t.startsWith("@") ? t.slice(1, t.indexOf("/")) : "");
const addrOf = (t: string) => (t.startsWith("@") ? t.slice(t.indexOf("/") + 1) : t);
const isHttps = (t: string) => addrOf(t).startsWith("https://");
// The address without its scheme and without the scheme's default port.
const pretty = (t: string) => {
  const a = addrOf(t).replace(/^https:\/\//, "");
  return a.replace(isHttps(t) ? /:443$/ : /:80$/, "");
};

export default function ProxyPage() {
  const [info, setInfo] = useState<DomainInfo | null>(null);
  const [busy, setBusy] = useState("");
  const [dv, setDv] = useState("");
  const [nodes, setNodes] = useState<string[]>([]); // registered nodes, for the picker
  const [rn, setRn] = useState(""); // route subdomain
  const [rnode, setRnode] = useState(""); // node it goes through ("" = the primary remote)
  const [rhost, setRhost] = useState(""); // host as the node sees it ("" = the node itself)
  const [rport, setRport] = useState("");
  const [rhttps, setRhttps] = useState(false); // speak TLS to the target

  const load = useCallback(async () => {
    try {
      setInfo(await api.domains());
    } catch { /* keep the last view */ }
  }, []);
  useEffect(() => {
    load();
    const t = setInterval(load, 2000);
    return () => clearInterval(t);
  }, [load]);
  useEffect(() => {
    const loadNodes = () => api.nodes().then((l) => setNodes(l.nodes.map((n) => n.name))).catch(() => {});
    loadNodes();
    const t = setInterval(loadNodes, 5000);
    return () => clearInterval(t);
  }, []);

  // One privileged or restarting action at a time: they can take a while (the
  // system password dialog) and must not overlap.
  const act = async (key: string, label: string, fn: () => Promise<unknown>) => {
    setBusy(key);
    try {
      await fn();
      toast.success(label);
    } catch (e) {
      const m = errMsg(e);
      if (m !== "cancelled") toast.error(m);
    } finally {
      setBusy("");
      load();
    }
  };

  const domain = info?.domains[0] ?? "";
  const setDomain = () => {
    const d = dv.replace(/^\.+|\.+$/g, "");
    if (!validDomain(d)) return toast.error("Use a name like mole.lan or dev.test");
    act("domain", `domain ${d}`, () => api.domainSet(d)).then(() => setDv(""));
  };
  const addRoute = () => {
    const name = rn.trim();
    if (!/^[a-z0-9]+(-[a-z0-9]+)*$/.test(name)) return toast.error("Subdomain: lowercase letters, digits and '-', e.g. web");
    if (!domain) return toast.error("Set a domain first");
    if (rhost && (!/^[a-z0-9.-]+$/.test(rhost) || /^\d+$/.test(rhost))) return toast.error("Host: a name or IP the chosen machine can reach");
    const port = Number(rport);
    // A name can use the scheme's default port; "this machine" needs one.
    if (rport ? !Number.isInteger(port) || port < 1 || port > 65535 : !rhost) return toast.error(rhost ? "Port must be 1–65535" : "Give a port, or a host to use the default one");
    const addr = (rhttps ? "https://" : "") + (rhost || (rhttps ? "127.0.0.1" : "")) + (rhost ? (rport ? `:${port}` : "") : (rhttps ? `:${port}` : String(port)));
    const target = rnode ? `@${rnode}/${addr}` : addr;
    act("route", `route ${name}.${domain}`, () => api.routeAdd(name, target)).then(() => { setRn(""); setRhost(""); setRport(""); });
  };

  // Machines a route can go through: the primary remote, then the nodes. A
  // node that is the primary remote under another name would only open a
  // second tunnel to the same host, so it is left out.
  const primary = info?.remote ?? "";
  const pickable = nodes.filter((n) => n !== primary);
  const field = "no-drag border border-line bg-transparent px-3 py-2 text-xs tracking-[0.15em] text-foreground outline-none placeholder:text-faint select-text focus:border-hot";
  const chip = (on: boolean) => `no-drag border px-3 py-2 text-[11px] tracking-[0.2em] transition ${on ? "border-hot bg-hot font-bold text-background" : "border-line text-dim hover:border-foreground hover:text-foreground"}`;
  const submitOnEnter = (e: React.KeyboardEvent) => e.key === "Enter" && busy === "" && addRoute();

  const dns = info?.dns;
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <SectionHead title="DOMAIN" hint="NAMES FOR REMOTE SERVICES LIVE UNDER IT" />
      <InputRow value={dv} onChange={setDv} onSubmit={setDomain} placeholder={domain ? `CHANGE DOMAIN (NOW ${domain})` : "SET DOMAIN, E.G. MOLE.LAN"} pattern={/[^a-z0-9.-]/g} action="SET" busy={busy !== ""} />
      <div className="border-b border-line px-5 py-4 text-xs text-dim">
        {domain ? (
          <span>
            <span className="font-display text-2xl font-bold text-foreground">{domain}</span>
            <span className="ml-4 text-faint">ANY NAME.{domain.toUpperCase()} CAN BE A ROUTE</span>
          </span>
        ) : (
          <span className="tracking-[0.3em] text-faint">NO DOMAIN SET</span>
        )}
        {info?.warning && <div className="mt-2 text-warn">{info.warning}</div>}
      </div>

      <SectionHead title="DNS" hint="MAKES THE SYSTEM RESOLVE THE DOMAIN THROUGH MOLE" />
      <div className="flex items-center border-b border-line px-5 py-4">
        <span className={`mr-4 size-2 ${dns?.installed ? "bg-live" : "bg-faint"}`} />
        <span className="w-48 font-display text-2xl font-bold">{dns?.installed ? "INSTALLED" : dns?.supported === false ? "UNSUPPORTED" : "NOT INSTALLED"}</span>
        <span className="flex-1 text-xs text-dim">
          {dns?.installed ? "Routes open without a port." : dns?.detail || ""}
          {dns?.supported && !dns.installed && domain && <span className="text-faint"> · ASKS FOR YOUR PASSWORD ONCE</span>}
        </span>
        {dns?.supported && domain && (
          <div className="no-drag flex">
            <button disabled={busy !== ""} onClick={() => act("dns", dns.installed ? "reinstalled" : "installed", api.dnsInstall)} className="border border-line px-5 py-2 text-[11px] tracking-[0.2em] transition enabled:hover:bg-foreground enabled:hover:text-background disabled:opacity-40">
              {busy === "dns" ? "WORKING…" : dns.installed ? "REINSTALL" : "INSTALL"}
            </button>
            {dns.installed && (
              <button disabled={busy !== ""} onClick={() => act("dns", "uninstalled", api.dnsUninstall)} className="-ml-px border border-line px-5 py-2 text-[11px] tracking-[0.2em] text-bad transition enabled:hover:bg-bad enabled:hover:text-background disabled:opacity-40">
                UNINSTALL
              </button>
            )}
          </div>
        )}
      </div>

      <SectionHead title="ROUTES" hint="SEVERAL SUBDOMAINS CAN SHARE ONE TARGET" />
      <div className="no-drag border-b border-line">
        <div className="flex items-stretch">
          <div className="flex items-center px-5 text-hot"><Plus size={20} /></div>
          <input value={rn} onChange={(e) => setRn(e.target.value.toLowerCase().replace(/[^a-z0-9-]/g, ""))} onKeyDown={submitOnEnter} placeholder="SUBDOMAIN" spellCheck={false} aria-label="Subdomain" className="min-w-0 flex-1 bg-transparent py-5 font-display text-3xl font-bold tracking-wide text-foreground outline-none placeholder:text-faint select-text" />
          <span className="flex items-center pr-5 font-display text-2xl font-bold text-faint">.{domain || "domain"}</span>
        </div>
        <div className="space-y-3 border-t border-line bg-white/[0.02] px-5 py-4">
          <div className="flex flex-wrap items-center gap-2">
            <span className="mr-2 w-20 text-[10px] tracking-[0.25em] text-faint">GOES TO</span>
            <button onClick={() => setRnode("")} aria-pressed={rnode === ""} className={chip(rnode === "")}>
              {primary ? primary.toUpperCase() : "DEFAULT"} <span className="opacity-60">· DEFAULT</span>
            </button>
            {pickable.map((n) => (
              <button key={n} onClick={() => setRnode(n)} aria-pressed={rnode === n} className={chip(rnode === n)}>{n.toUpperCase()}</button>
            ))}
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <span className="mr-2 w-20 text-[10px] tracking-[0.25em] text-faint">TARGET</span>
            <button onClick={() => setRhttps(false)} aria-pressed={!rhttps} className={chip(!rhttps)}>HTTP</button>
            <button onClick={() => setRhttps(true)} aria-pressed={rhttps} className={`${chip(rhttps)} -ml-px`}>HTTPS</button>
            <input value={rhost} onChange={(e) => setRhost(e.target.value.toLowerCase().replace(/[^a-z0-9.-]/g, ""))} onKeyDown={submitOnEnter} placeholder="LOCALHOST, OR A NAME LIKE ADMIN.CORP" spellCheck={false} aria-label="Host" className={`${field} min-w-0 flex-1`} />
            <span className="text-faint">:</span>
            <input value={rport} onChange={(e) => setRport(e.target.value.replace(/\D/g, "").slice(0, 5))} onKeyDown={submitOnEnter} placeholder={rhost ? (rhttps ? "443" : "80") : "3000"} inputMode="numeric" aria-label="Port" className={`${field} w-24`} />
            <button disabled={busy !== ""} onClick={addRoute} className="ml-2 bg-hot px-8 py-2 font-display text-sm font-bold tracking-[0.2em] text-background transition enabled:hover:brightness-110 enabled:active:translate-y-px disabled:opacity-40">
              ADD
            </button>
          </div>
        </div>
      </div>
      {(info?.routes ?? []).map((r, i) => (
        <div key={r.name} style={{ animationDelay: `${i * 30}ms` }} className="rise group flex items-center border-b border-line px-5 py-4 transition hover:bg-white/[0.03]">
          <span className={`mr-4 size-2 ${info?.running ? "bg-live" : "bg-faint"}`} />
          <span className="min-w-56 font-display text-3xl font-bold">{r.hosts[0] ?? r.name}</span>
          <span className="flex flex-1 items-center gap-3 text-xs text-dim">
            <ArrowRight size={14} className="text-hot" />
            <span className={`border px-2 py-0.5 text-[10px] tracking-[0.2em] ${node(r.target) ? "border-hot text-hot" : "border-line text-dim"}`}>{(node(r.target) || primary || "REMOTE").toUpperCase()}</span>
            {isHttps(r.target) && <span className="border border-live px-2 py-0.5 text-[10px] tracking-[0.2em] text-live">HTTPS</span>}
            {pretty(r.target)}
          </span>
          <button onClick={() => act("open", "", () => api.routeOpen(r.name))} title={r.url} className="no-drag mr-6 flex items-center gap-2 text-[10px] tracking-[0.2em] text-dim transition hover:text-foreground">
            OPEN <ExternalLink size={13} />
          </button>
          <button aria-label={`Remove route ${r.name}`} onClick={() => act("route", `− ${r.name}`, () => api.routeRemove(r.name))} className="no-drag p-1 text-faint transition hover:text-bad">
            <X size={18} />
          </button>
        </div>
      ))}
      {info && info.routes.length === 0 && <div className="p-8 text-xs tracking-[0.3em] text-faint">NO ROUTES</div>}
      {info && !info.running && info.routes.length > 0 && <div className="px-5 py-4 text-[10px] tracking-[0.25em] text-faint">DAEMON DOWN: ROUTES ANSWER ONCE IT STARTS</div>}
    </div>
  );
}
