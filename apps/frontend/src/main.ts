import "@xterm/xterm/css/xterm.css";
import "./style.css";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { shellPrompt, type FromWorker, type NetState, type ToWorker, type VMState } from "@bootlet/shared";

const statusEl = document.getElementById("status")!;
const statusText = document.getElementById("status-text")!;
const toggleBtn = document.getElementById("toggle") as HTMLButtonElement;
const restartBtn = document.getElementById("restart") as HTMLButtonElement;
const resetDiskBtn = document.getElementById("reset-disk") as HTMLButtonElement;
const netEl = document.getElementById("net")!;

// ?mem=256 sets the guest RAM in MiB and ?quiet hides the kernel's boot log.
const params = new URLSearchParams(location.search);
const memMiB = Number(params.get("mem")) || 0;
const quiet = params.has("quiet");

// The network relay normally runs on the user's own computer (apps/relay).
const LOCAL_RELAY = "ws://127.0.0.1:8089/";
const RELAY_KEY = "bootlet.relay";
const RELEASES = "https://github.com/bpradana/bootlet/releases/latest/download/bootlet-relay-";
let relay = "";
let servedByRelay = false;

const remembered = {
  get: () => {
    try {
      return localStorage.getItem(RELAY_KEY) ?? "";
    } catch {
      return "";
    }
  },
  set: (url: string) => {
    try {
      if (url) localStorage.setItem(RELAY_KEY, url);
      else localStorage.removeItem(RELAY_KEY);
    } catch {
      // storage unavailable: the choice just isn't remembered
    }
  },
};

// resolveRelay picks the relay to start with, in order: ?relay= (empty for
// none); the relay that served this page (it answers relay.json); a
// build-time default; the last relay the user connected to. Otherwise there
// is none until the user connects from the network dialog: auto-connecting
// to localhost from a public site would greet every visitor with a browser
// permission prompt.
async function resolveRelay(): Promise<string> {
  const param = params.get("relay");
  if (param !== null) return param;
  try {
    const res = await fetch("./relay.json", { cache: "no-store" });
    if (res.ok && (await res.json()).relay === "same-origin") {
      servedByRelay = true;
      return `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/`;
    }
  } catch {
    // not served by a relay
  }
  return import.meta.env.VITE_RELAY_URL || remembered.get();
}

const term = new Terminal({
  fontFamily: 'ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
  fontSize: 14,
  cursorBlink: true,
  scrollback: 5000,
  theme: { background: "#0d1117", foreground: "#e6edf3", cursor: "#3fb950", selectionBackground: "#264f78" },
});
const fit = new FitAddon();
term.loadAddon(fit);
term.open(document.getElementById("terminal")!);
fit.fit();
new ResizeObserver(() => fit.fit()).observe(document.getElementById("terminal")!);

const worker = new Worker(new URL("./worker.ts", import.meta.url), { type: "module" });
const send = (msg: ToWorker) => worker.postMessage(msg);

const encoder = new TextEncoder();
const decoder = new TextDecoder();
term.onData((s) => send({ type: "input", data: encoder.encode(s) }));

let state: VMState = "loading";
let booted = false; // shell prompt seen since the last (re)boot
let mips = 0;
let progressLine = false;
let recent = ""; // tail of console output, to spot the prompt across chunks
let persistent = false;

const netLabels: Record<NetState, string> = {
  off: "Network off",
  connecting: "Connecting…",
  online: "Online",
  offline: "Offline",
};

function render() {
  statusEl.dataset.state = state === "running" && !booted ? "booting" : state;
  let text: string;
  switch (state) {
    case "loading":
      text = "Downloading…";
      break;
    case "booting":
      text = "Booting…";
      break;
    case "running":
      text = !booted ? `Booting… ${mips.toFixed(0)} MIPS` : mips >= 1 ? `Running · ${mips.toFixed(0)} MIPS` : "Running · idle";
      break;
    case "stopped":
      text = "Stopped";
      break;
    case "halted":
      text = "Powered off";
      break;
    case "error":
      text = "Error";
      break;
  }
  statusText.textContent = text;
  toggleBtn.disabled = state === "loading" || state === "error";
  toggleBtn.textContent = state === "stopped" || state === "halted" ? "Start" : "Stop";
  restartBtn.disabled = state === "loading" || state === "error";
  resetDiskBtn.disabled = restartBtn.disabled || !persistent;
}

const mb = (n: number) => (n / 1048576).toFixed(1);

worker.onmessage = (ev: MessageEvent<FromWorker>) => {
  const msg = ev.data;
  switch (msg.type) {
    case "progress": {
      // With Content-Encoding the length header is the compressed size.
      const total = msg.total >= msg.loaded ? `/${mb(msg.total)}` : "";
      term.write(`\r\x1b[2K\x1b[90mdownloading ${msg.label} ${mb(msg.loaded)}${total} MB\x1b[0m`);
      progressLine = true;
      break;
    }
    case "output": {
      if (progressLine) {
        term.write("\r\x1b[2K");
        progressLine = false;
      }
      term.write(msg.data);
      if (!booted) {
        recent = (recent + decoder.decode(msg.data, { stream: true })).slice(-256);
        if (recent.includes(shellPrompt)) {
          booted = true;
          render();
        }
      }
      break;
    }
    case "storage":
      persistent = msg.backend !== "none";
      statusEl.title = persistent
        ? `Filesystem changes are saved in this browser (${msg.backend === "opfs" ? "Origin Private File System" : "IndexedDB"})`
        : `Changes are not saved: ${msg.detail ?? "browser storage unavailable"}`;
      render();
      break;
    case "net":
      netState = msg.state;
      netEl.dataset.state = msg.state;
      netEl.textContent = netLabels[msg.state];
      netEl.title = msg.state === "off" ? "Set up networking" : `Relay: ${relay}`;
      if (msg.state === "online" && !servedByRelay) remembered.set(relay);
      renderNetDialog();
      break;
    case "stats":
      mips = msg.mips;
      render();
      break;
    case "state":
      state = msg.state;
      if (state === "booting") {
        booted = false;
        recent = "";
        term.write("\r\x1b[2K\x1b[90mbooting Linux…\x1b[0m");
        progressLine = true;
      }
      if (state === "halted") term.write("\r\n\x1b[90m[machine powered off: press Start to boot again]\x1b[0m\r\n");
      if (state === "error") term.write(`\r\n\x1b[31m[error] ${msg.detail ?? "unknown"}\x1b[0m\r\n`);
      render();
      break;
  }
};

toggleBtn.onclick = () => {
  if (state === "halted") {
    restart();
  } else if (state === "stopped") {
    send(startMessage());
  } else {
    send({ type: "stop" });
  }
  term.focus();
};

function restart() {
  term.reset();
  send({ type: "restart" });
}

restartBtn.onclick = () => {
  restart();
  term.focus();
};

resetDiskBtn.onclick = () => {
  if (!confirm("Wipe the saved filesystem (installed packages, files, settings) and reboot from a clean system?")) return;
  term.reset();
  send({ type: "resetDisk" });
  term.focus();
};

// Network dialog.
const netDialog = document.getElementById("net-dialog") as HTMLDialogElement;
const netStatus = document.getElementById("net-status")!;
const netSetup = document.getElementById("net-setup")!;
const netURL = document.getElementById("net-url") as HTMLInputElement;
const netConnect = document.getElementById("net-connect") as HTMLButtonElement;
const netDisconnect = document.getElementById("net-disconnect") as HTMLButtonElement;
let netState: NetState = "off";

const downloads: [string, string][] = [
  ["macOS (Apple silicon)", "darwin-arm64"],
  ["macOS (Intel)", "darwin-amd64"],
  ["Linux (x86-64)", "linux-amd64"],
  ["Linux (ARM64)", "linux-arm64"],
  ["Windows", "windows-amd64.exe"],
];
document.getElementById("net-downloads")!.append(
  ...downloads.flatMap(([label, file], i) => {
    const a = Object.assign(document.createElement("a"), { href: RELEASES + file, textContent: label });
    return i === 0 ? [a] : [document.createTextNode(" · "), a];
  }),
);

function renderNetDialog() {
  const statusText: Record<NetState, string> = {
    off: "Not connected. The guest is offline.",
    connecting: `Connecting to ${relay}…`,
    online: servedByRelay ? "Online through the relay that serves this page." : `Online through ${relay}.`,
    offline: `Can't reach ${relay}. Is the relay running? Retrying automatically.`,
  };
  netStatus.textContent = statusText[netState];
  netSetup.hidden = netState === "online";
  netDisconnect.disabled = netState === "off";
}

netEl.onclick = () => {
  netURL.value = relay || LOCAL_RELAY;
  renderNetDialog();
  netDialog.showModal();
};
netConnect.onclick = () => {
  relay = netURL.value.trim();
  send({ type: "relay", url: relay });
};
netDisconnect.onclick = () => {
  relay = "";
  remembered.set("");
  send({ type: "relay", url: "" });
};
netDialog.addEventListener("close", () => term.focus());

function startMessage(): ToWorker {
  return { type: "start", base: document.baseURI, relay, memMiB, quiet };
}

render();
void resolveRelay().then((url) => {
  relay = url;
  send(startMessage());
});
term.focus();
