import { MoleService, type Status, type Events, type CLIStatus, type TransferView, type SendResult, type NodeList as RawNodeList, type NodeInfo as RawNodeInfo, type NodeDaemon as RawNodeDaemon, type NodeSpec, type NodeEntry, type NodeJobView, type NodeRunResult, type SSHHost, type AppUpdate, type UpdateProgress, type DomainInfo as RawDomainInfo } from "../bindings/github.com/Luqueee/mole/desktop";
import type { PushInfo } from "../bindings/github.com/Luqueee/mole/internal/admin";
import type { Active, Pending, Record as TRecord } from "../bindings/github.com/Luqueee/mole/internal/transfer";

// TransferView with the nulls Go emits for empty slices turned into lists.
export type Transfers = { supported: boolean; last: number; records: TRecord[]; active: Active[]; pending: Pending[] };
const normalize = (v: TransferView): Transfers => ({ supported: v.supported, last: v.last, records: v.records ?? [], active: v.active ?? [], pending: v.pending ?? [] });

// Nodes with the nulls Go emits for empty slices turned into lists.
export type NodeDaemon = Omit<RawNodeDaemon, "ports" | "push" | "discoverPorts" | "excludePorts"> & { ports: number[]; push: PushInfo[]; discoverPorts: number[]; excludePorts: number[] };
export type NodeInfo = Omit<RawNodeInfo, "daemon"> & { daemon: NodeDaemon | null };
export type NodeList = Omit<RawNodeList, "nodes"> & { nodes: NodeEntry[] };
const normalizeNodeInfo = (i: RawNodeInfo): NodeInfo => ({
  ...i,
  daemon: i.daemon ? { ...i.daemon, ports: i.daemon.ports ?? [], push: i.daemon.push ?? [], discoverPorts: i.daemon.discoverPorts ?? [], excludePorts: i.daemon.excludePorts ?? [] } : null,
});

// DomainInfo with the nulls Go emits for empty slices turned into lists.
type RawRoute = NonNullable<RawDomainInfo["routes"]>[number];
export type DomainInfo = Omit<RawDomainInfo, "domains" | "routes"> & { domains: string[]; routes: (Omit<RawRoute, "hosts"> & { hosts: string[] })[] };

// Browser-only design mode (?mock): the Wails runtime is absent, so the UI
// is fed synthetic data. The desktop app never takes this path.
export const MOCK = new URLSearchParams(location.search).has("mock");

const t0 = Date.now();
const mockPorts = [3007, 3011, 3013, 3389, 3390, 5355, 9224, 11434, 35005, 38907, 40261, 44481, 45567, 46299, 54001, 54002, 54003, 55301, 55437, 55438];
let mockDiscover = [3000, 5173, 8080, 9229, 4321, 6006];
let mockPush = [
  { local: 3000, remote: 8080, bind: "127.0.0.1", state: "active", error: "" },
  { local: 5432, remote: 5432, bind: "127.0.0.1", state: "failed", error: "remote port 5432 in use" },
];

function mockStatus(): Status {
  const s = (Date.now() - t0) / 1000;
  const wob = (k: number, a: number) => Math.round(a + a * Math.sin(s / k));
  return {
    running: true,
    adminAddr: "127.0.0.1:9999",
    remote: "devlabs",
    stats: {
      uptime: "147h27m" + String(Math.floor(s) % 60).padStart(2, "0") + "s",
      active_conns: 2 + wob(3, 3),
      total_conns: 16734 + Math.floor(s * 1.7),
      failed_dials: 1445 + Math.floor(s / 9),
    },
    ports: mockPorts,
    push: mockPush,
    info: {},
  } as Status;
}

const mockLogs = [
  'time=2026-10-22T18:55:35.812+02:00 level=INFO msg="ssh connected" remote=devlabs',
  'time=2026-10-22T18:55:35.812+02:00 level=INFO msg=forwarding local=127.0.0.1:3000 remote=devlabs:3000',
  'time=2026-10-22T18:55:35.812+02:00 level=INFO msg=forwarding local=127.0.0.1:5173 remote=devlabs:5173',
  'time=2026-10-22T18:55:35.813+02:00 level=WARN msg="could not bind local port, skipping" port=43159 err="address already in use"',
  'time=2026-10-22T18:55:50.812+02:00 level=INFO msg=forwarding local=127.0.0.1:8080 remote=devlabs:8080',
  'time=2026-10-22T18:56:05.833+02:00 level=ERROR msg="dial failed" remote=devlabs:9229 err="connection refused"',
  'time=2026-10-22T18:56:20.101+02:00 level=DEBUG msg="discover tick" listeners=5 forwarded=5',
  'time=2026-10-22T18:56:35.833+02:00 level=INFO msg=forwarding local=127.0.0.1:38485 remote=devlabs:38485',
];

let mockId = 0;
let mockNext = 0;
function mockEvents(): Events {
  const out: Events["events"] = [];
  const now = Date.now();
  if (!mockNext) mockNext = now + 800;
  if (now >= mockNext) {
    mockNext = now + 500 + Math.random() * 900;
    const push = Math.random() < 0.3;
    const port = push ? 3000 : [3011, 3013, 3011, 3007, 11434][Math.floor(Math.random() * 5)];
    out.push({ id: ++mockId, dir: push ? "push" : "pull", port, startMs: now, durMs: 4 + Math.round(Math.random() * 60), bytesOut: 380, bytesIn: 800 + Math.round(Math.random() * 9000), failed: Math.random() < 0.12 });
  }
  return { supported: true, last: mockId, events: out } as Events;
}

// Mock transfers: one offer waiting, one file in flight, a little history.
let mockReceive = true;
let mockPending = [{ id: 7, peer: "devlabs", to: "", files: [{ name: "build-2026-10-24.tar.gz", size: 48_300_000, mode: 420, mtime: 0 }, { name: "notes.md", size: 2_100, mode: 420, mtime: 0 }], total: 48_302_100, at_ms: Date.now() - 20_000 }];
const mockT0 = Date.now();
function mockTransfers(): TransferView {
  const now = Date.now();
  const sent = Math.min(((now - mockT0) % 20_000) / 20_000, 1) * 120_000_000;
  return {
    supported: true,
    last: 3,
    records: [
      { id: 1, dir: "send", peer: "devlabs", name: "schema.sql", saved: "schema.sql", size: 84_000, bytes: 84_000, start_ms: now - 3_600_000, dur_ms: 90, status: "ok" },
      { id: 2, dir: "recv", peer: "devlabs", name: "core.dump", saved: "core (1).dump", size: 912_000_000, bytes: 304_000_000, start_ms: now - 900_000, dur_ms: 41_000, status: "failed", error: "write: no space left on device" },
      { id: 3, dir: "recv", peer: "devlabs", name: "wallpaper.png", size: 3_400_000, bytes: 0, start_ms: now - 120_000, dur_ms: 0, status: "rejected", error: "rejected by the user" },
    ],
    active: [{ id: 4, dir: "send", peer: "devlabs", name: "dataset.parquet", size: 120_000_000, bytes: Math.round(sent), start_ms: now - 4_000 }],
    pending: mockPending,
  } as TransferView;
}

let mockCli = false;
let mockPathSet = false;
function mockCliStatus(): CLIStatus {
  return { bundled: true, installed: mockCli, path: mockCli ? "/Users/me/.local/bin/mole" : "", version: mockCli ? "v0.2.0" : "", upToDate: mockCli, installDir: "/Users/me/.local/bin", dirOnPath: false, pathSet: mockPathSet, profile: "/Users/me/.zshrc", managedBy: "" } as CLIStatus;
}

// ───────── nodes (mock) ─────────

const mockSelf = { hostname: "adria-mbp", os: "darwin", arch: "arm64" };
let mockNodeEntries: NodeEntry[] = [
  { name: "devlabs", target: "adria@devlabs.internal", sshPort: 0, bin: "", insecure: false, implicit: false },
  { name: "buildbox", target: "ci@10.0.4.21", sshPort: 2222, bin: "~/apps/mole", insecure: false, implicit: false },
  { name: "pi-garage", target: "pi@raspberrypi.local", sshPort: 0, bin: "", insecure: false, implicit: false },
  { name: "old-vps", target: "root@203.0.113.9", sshPort: 0, bin: "", insecure: true, implicit: false },
  { name: "fresh-box", target: "dev@10.0.4.77", sshPort: 0, bin: "", insecure: false, implicit: false },
];
const mockDaemon = (over: Partial<NodeDaemon>): NodeDaemon => ({
  running: true, uptime: "52h11m", activeConns: 3, totalConns: 9_418, failedDials: 12, ports: [3000, 5173, 8080], push: [{ local: 4000, remote: 4000, bind: "127.0.0.1", state: "active", error: "" }],
  remote: "adria-mbp", adminAddr: "127.0.0.1:9999", configPath: "/home/adria/.config/mole/config.yaml", inbox: "/home/adria/mole-inbox", receive: true, accept: "auto", nodes: 2,
  autoDiscover: true, discoverPorts: [3000, 5173, 8080, 9229], excludePorts: [22, 25, 53], ...over,
});
const mockProbe = (name: string): NodeInfo => {
  const e = mockNodeEntries.find((n) => n.name === name);
  const base = { name, target: e?.target ?? name, sshPort: e?.sshPort ?? 0, bin: e?.bin ?? "", insecure: e?.insecure ?? false, implicit: e?.implicit ?? false, error: "", agent: 0, connectMs: 0, roundTripMs: 0, build: "", version: "", os: "", arch: "", hostname: "", cliOutdated: false, daemon: null };
  const jitter = (a: number) => Math.round((a + Math.random() * a * 0.25) * 10) / 10;
  switch (name) {
    case "devlabs": return { ...base, state: "ready", version: "0.4.0", build: "main@97b20cb", os: "linux", arch: "amd64", hostname: "devlabs", agent: 1, connectMs: jitter(38), roundTripMs: jitter(4), daemon: mockDaemon({}) };
    case "buildbox": return { ...base, state: "ready", version: "0.4.0", build: "feat/nodes-ui@a1b2c3d", os: "linux", arch: "arm64", hostname: "buildbox-01", agent: 1, connectMs: jitter(120), roundTripMs: jitter(9), daemon: mockDaemon({ running: false, uptime: "", ports: [], push: [], activeConns: 0, totalConns: 0, failedDials: 0, receive: false, discoverPorts: [3000, 9000] }) };
    case "pi-garage": return { ...base, state: "no-agent", version: "0.3.0", connectMs: jitter(210) };
    case "fresh-box": return { ...base, state: mockInstalled ? "ready" : "not-installed", error: mockInstalled ? "" : "mole is not installed there (or not in ~/.local/bin or /usr/local/bin): `mole node install NAME` installs it, or register the node with -bin PATH", version: mockInstalled ? "0.5.0" : "", os: mockInstalled ? "linux" : "", arch: mockInstalled ? "amd64" : "", hostname: "fresh-box", agent: mockInstalled ? 1 : 0, connectMs: jitter(70), roundTripMs: mockInstalled ? jitter(5) : 0, daemon: mockInstalled ? mockDaemon({ running: false, uptime: "", ports: [], push: [], activeConns: 0, totalConns: 0, failedDials: 0, receive: false, discoverPorts: [] }) : null };
    default: return { ...base, state: "unreachable", error: "dial tcp 203.0.113.9:22: i/o timeout" };
  }
};
let mockInstalled = false;
const mockSleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
let mockJobs: Record<number, { text: string; done: boolean; code: number }> = {};
let mockJobId = 0;
function mockJob(lines: [number, string][], code = 0): number {
  const id = ++mockJobId;
  mockJobs[id] = { text: "", done: false, code: 0 };
  let at = 0;
  lines.forEach(([ms, line], i) => {
    at += ms;
    setTimeout(() => {
      const j = mockJobs[id];
      if (!j || j.done) return;
      j.text += line + "\n";
      if (i === lines.length - 1) { j.done = true; j.code = code; }
    }, at);
  });
  return id;
}

let mockUpdateAt = 0;
const mockUpdate = (): AppUpdate => ({ current: "0.4.0", latest: "0.5.0", available: true, page: "https://github.com/Luqueee/mole/releases/tag/desktop-v0.5.0", asset: "mole.dmg", size: 62_000_000, installable: true, error: "", checkedMs: Date.now() });
const mockProgress = (): UpdateProgress => {
  if (!mockUpdateAt) return { phase: "idle", bytes: 0, total: 0, path: "", error: "" };
  const f = Math.min((Date.now() - mockUpdateAt) / 4000, 1);
  return f < 1 ? { phase: "downloading", bytes: Math.round(f * 62_000_000), total: 62_000_000, path: "", error: "" } : { phase: "opened", bytes: 62_000_000, total: 62_000_000, path: "/Users/me/Library/Caches/mole/updates/mole.dmg", error: "" };
};

// ───────── domains (mock) ─────────

let mockDomain = "mole.lan";
let mockDns = false;
let mockRoutes = [{ name: "web", target: "127.0.0.1:3000" }, { name: "api", target: "10.0.0.5:8080" }, { name: "db", target: "@buildbox/127.0.0.1:5432" }, { name: "admin", target: "https://admin.corp:443" }];
const mockDomainInfo = (): DomainInfo => ({
  remote: "devlabs",
  domains: mockDomain ? [mockDomain] : [],
  routes: mockRoutes.map((r) => {
    const host = `${r.name}.${mockDomain}`;
    return { name: r.name, hosts: mockDomain ? [host] : [], target: r.target, url: mockDns ? `http://${host}` : `http://${host}:7080` };
  }),
  routePort: 7080,
  running: true,
  warning: /\.(local|dev|app)$/.test(mockDomain) ? `.${mockDomain.split(".").pop()} is a poor choice: use .lan or .test` : "",
  dns: { supported: true, installed: mockDns && !!mockDomain, detail: mockDns ? "" : `missing: /etc/resolver/${mockDomain}, pf redirect` },
});

export const api = {
  appVersion: async (): Promise<string> => (MOCK ? "0.4.0" : MoleService.GetAppVersion()),
  appCheck: async (force: boolean): Promise<AppUpdate> => (MOCK ? mockUpdate() : MoleService.CheckAppUpdate(force)),
  appDownload: async () => (MOCK ? void (mockUpdateAt = Date.now()) : MoleService.DownloadAppUpdate()),
  appProgress: async (): Promise<UpdateProgress> => (MOCK ? mockProgress() : MoleService.GetUpdateProgress()),
  appPage: async (url: string) => (MOCK ? void window.open(url) : MoleService.OpenReleasePage(url)),
  nodes: async (): Promise<NodeList> =>
    MOCK ? { self: mockSelf, nodes: mockNodeEntries, cliReady: true, cliError: "" } : ((l) => ({ ...l, nodes: l.nodes ?? [] }))(await MoleService.GetNodes()),
  probe: async (name: string): Promise<NodeInfo> => {
    if (MOCK) { await mockSleep(300 + Math.random() * 900); return mockProbe(name); }
    return normalizeNodeInfo(await MoleService.ProbeNode(name));
  },
  nodeAdd: async (spec: NodeSpec) => {
    if (!MOCK) return MoleService.AddNode(spec);
    await mockSleep(900);
    if (spec.check && spec.name === "fresh") throw new Error(`cannot reach ${spec.target}: mole is not installed there (or not in ~/.local/bin or /usr/local/bin): \`mole node install NAME\` installs it, or register the node with -bin PATH`);
    if (spec.name === "fail") throw new Error(`cannot reach ${spec.target}: dial tcp: i/o timeout\n(use -no-check to register it anyway)`);
    mockNodeEntries = [...mockNodeEntries.filter((n) => n.name !== spec.name), { name: spec.name, target: spec.target, sshPort: spec.sshPort, bin: spec.bin, insecure: spec.insecure, implicit: false }];
  },
  nodeRemove: async (name: string) => (MOCK ? void (mockNodeEntries = mockNodeEntries.filter((n) => n.name !== name)) : MoleService.RemoveNode(name)),
  nodeRun: async (name: string, args: string[]): Promise<NodeRunResult> => (MOCK ? (await mockSleep(500), { output: `ran: mole -n ${name} ${args.join(" ")}`, code: 0 }) : MoleService.RunOnNode(name, args)),
  nodePortAdd: async (name: string, port: number) => (MOCK ? mockSleep(400) : MoleService.NodeAddPort(name, port)),
  nodePortRemove: async (name: string, port: number) => (MOCK ? mockSleep(400) : MoleService.NodeRemovePort(name, port)),
  nodePushAdd: async (name: string, local: number, remote: number) => (MOCK ? mockSleep(400) : MoleService.NodeAddPush(name, local, remote)),
  nodePushRemove: async (name: string, local: number) => (MOCK ? mockSleep(400) : MoleService.NodeRemovePush(name, local)),
  nodeStart: async (name: string) => (MOCK ? mockSleep(800) : MoleService.NodeStart(name)),
  nodeStop: async (name: string) => (MOCK ? mockSleep(800) : MoleService.NodeStop(name)),
  nodeRestart: async (name: string) => (MOCK ? mockSleep(800) : MoleService.NodeRestart(name)),
  jobInstall: async (name: string, branch: string): Promise<number> => {
    if (!MOCK) return MoleService.StartNodeInstall(name, branch);
    setTimeout(() => { mockInstalled = true; }, 4600);
    return mockJob([[300, `installing mole on ${name} (${branch ? "branch " + branch : "latest main"})`], [500, "== Go is not installed"], [700, "== downloading Go 1.26.4 (linux/amd64)"], [1500, "== building mole from main (97b20cb)"], [1200, "installed ~/.local/bin/mole"], [200, `mole installed on ${name}: try \`mole node ping ${name}\``]]);
  },
  jobStart: async (name: string, args: string[]): Promise<number> => {
    if (!MOCK) return MoleService.StartNodeJob(name, args);
    if (args[0] === "update") return mockJob([[300, `→ updating mole on ${name}`], [700, "cloning https://github.com/Luqueee/mole (main)"], [900, "go build ./cmd/mole"], [1400, "installed ~/.local/bin/mole"], [200, "mole v0.4.0 (main@97b20cb)"]]);
    return mockJob([[300, `mole v0.4.0 · ${name}`], [200, args.join(" ")], [300, "ok"]]);
  },
  job: async (id: number, from: number): Promise<NodeJobView> => {
    if (!MOCK) return MoleService.GetNodeJob(id, from);
    const j = mockJobs[id];
    if (!j) return { found: false, output: "", next: 0, done: true, code: 0, error: "" };
    return { found: true, output: j.text.slice(from), next: j.text.length, done: j.done, code: j.code, error: "" };
  },
  jobCancel: async (id: number) => (MOCK ? void (mockJobs[id] && (mockJobs[id].done = true, mockJobs[id].code = -1)) : MoleService.CancelNodeJob(id)),
  splitCommand: async (line: string): Promise<string[]> => (MOCK ? line.trim().split(/\s+/).filter(Boolean) : ((await MoleService.SplitCommand(line)) ?? [])),
  sshHosts: async (): Promise<SSHHost[]> => (MOCK ? [{ alias: "devlabs", hostName: "devlabs.internal", user: "adria", port: 0 }, { alias: "staging", hostName: "staging.example.com", user: "deploy", port: 2200 }, { alias: "nas", hostName: "192.168.1.20", user: "admin", port: 0 }, { alias: "buildbox", hostName: "10.0.4.21", user: "ci", port: 2222 }] : ((await MoleService.GetSSHHosts()) ?? [])),
  transfers: async (since: number): Promise<Transfers> => normalize(MOCK ? mockTransfers() : await MoleService.GetTransfers(since)),
  pick: async (): Promise<string[]> => (MOCK ? [] : ((await MoleService.PickFiles()) ?? [])),
  send: async (paths: string[], to: string): Promise<SendResult[]> => (MOCK ? paths.map((name) => ({ name, saved: name, dir: "/home/dev/mole-inbox" })) : ((await MoleService.SendFiles(paths, to)) ?? [])),
  setFolder: async (to: string) => (MOCK ? undefined : MoleService.SetSendFolder(to)),
  decide: async (id: number, accept: boolean) => (MOCK ? void (mockPending = mockPending.filter((p) => p.id !== id)) : MoleService.ResolveTransfer(id, accept)),
  setReceive: async (on: boolean) => (MOCK ? void (mockReceive = on) : MoleService.SetReceive(on)),
  openInbox: async () => (MOCK ? undefined : MoleService.OpenInbox()),
  mockReceive: () => mockReceive,
  cli: async (): Promise<CLIStatus> => (MOCK ? mockCliStatus() : MoleService.GetCLI()),
  cliPath: async () => (MOCK ? void (mockPathSet = true) : MoleService.AddCLIToPath()),
  cliInstall: async () => (MOCK ? void (mockCli = true) : MoleService.InstallCLI()),
  events: async (since: number): Promise<Events> => (MOCK ? mockEvents() : MoleService.GetEvents(since)),
  status: async (): Promise<Status> => (MOCK ? mockStatus() : MoleService.GetStatus()),
  discover: async (): Promise<number[]> =>
    MOCK ? mockDiscover : ((await MoleService.GetSettings()).discoverPorts ?? []),
  logs: async (n: number): Promise<string[]> => (MOCK ? Array.from({ length: 12 }, () => mockLogs).flat() : ((await MoleService.GetLogs(n)) ?? [])),
  add: async (p: number) => (MOCK ? void (mockDiscover = [...mockDiscover, p]) : MoleService.AddDiscoverPort(p)),
  remove: async (p: number) =>
    MOCK ? void (mockDiscover = mockDiscover.filter((x) => x !== p)) : MoleService.RemoveDiscoverPort(p),
  pushAdd: async (local: number, remote: number) =>
    MOCK ? void (mockPush = [...mockPush, { local, remote: remote || local, bind: "127.0.0.1", state: "active", error: "" }]) : MoleService.AddPush(local, remote),
  pushRemove: async (local: number) =>
    MOCK ? void (mockPush = mockPush.filter((x) => x.local !== local)) : MoleService.RemovePush(local),
  domains: async (): Promise<DomainInfo> => {
    if (MOCK) return mockDomainInfo();
    const d = await MoleService.GetDomains();
    return { ...d, domains: d.domains ?? [], routes: (d.routes ?? []).map((r) => ({ ...r, hosts: r.hosts ?? [] })) };
  },
  domainSet: async (name: string) => (MOCK ? (await mockSleep(500), void ((mockDomain = name), (mockDns = false))) : MoleService.SetDomain(name)),
  routeAdd: async (name: string, target: string) => (MOCK ? void (mockRoutes = [...mockRoutes.filter((r) => r.name !== name), { name, target }]) : MoleService.AddRoute(name, target)),
  routeRemove: async (name: string) => (MOCK ? void (mockRoutes = mockRoutes.filter((r) => r.name !== name)) : MoleService.RemoveRoute(name)),
  routeOpen: async (name: string) => (MOCK ? void window.open(mockDomainInfo().routes.find((r) => r.name === name)?.url) : MoleService.OpenRoute(name)),
  dnsInstall: async () => (MOCK ? (await mockSleep(1500), void (mockDns = true)) : MoleService.InstallDNS()),
  dnsUninstall: async () => (MOCK ? (await mockSleep(1000), void (mockDns = false)) : MoleService.UninstallDNS()),
  start: async () => (MOCK ? undefined : MoleService.Start()),
  stop: async () => (MOCK ? undefined : MoleService.Stop()),
  restart: async () => (MOCK ? undefined : MoleService.Restart()),
};
