import { MoleService, type Status, type Events, type CLIStatus } from "../bindings/github.com/Luqueee/mole/desktop";

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

let mockCli = false;
let mockPathSet = false;
function mockCliStatus(): CLIStatus {
  return { bundled: true, installed: mockCli, path: mockCli ? "/Users/me/.local/bin/mole" : "", version: mockCli ? "v0.2.0" : "", upToDate: mockCli, installDir: "/Users/me/.local/bin", dirOnPath: false, pathSet: mockPathSet, profile: "/Users/me/.zshrc" } as CLIStatus;
}

export const api = {
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
  start: async () => (MOCK ? undefined : MoleService.Start()),
  stop: async () => (MOCK ? undefined : MoleService.Stop()),
  restart: async () => (MOCK ? undefined : MoleService.Restart()),
};
