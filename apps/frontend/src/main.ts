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

// ?relay=wss://host/ (or VITE_RELAY_URL at build time) enables networking
// through a Wisp relay; ?relay= with no value turns it off. ?mem=256 sets RAM,
// and ?quiet hides the kernel's boot log.
const params = new URLSearchParams(location.search);
const relay = params.get("relay") ?? import.meta.env.VITE_RELAY_URL ?? "";
const memMiB = Number(params.get("mem")) || 0;
const quiet = params.has("quiet");
const startMsg: ToWorker = { type: "start", base: document.baseURI, relay, memMiB, quiet };

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
      netEl.dataset.state = msg.state;
      netEl.textContent = netLabels[msg.state];
      netEl.title =
        msg.state === "off"
          ? "No network relay configured. Open the page with ?relay=wss://<relay>/ to enable networking."
          : `Wisp relay: ${relay}`;
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
    send(startMsg);
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

render();
send(startMsg);
term.focus();
