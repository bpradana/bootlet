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
const netStatusText = document.getElementById("net-status-text")!;
const netSetup = document.getElementById("net-setup")!;
const netOnline = document.getElementById("net-online")!;
const netURL = document.getElementById("net-url") as HTMLInputElement;
const netConnect = document.getElementById("net-connect") as HTMLButtonElement;
const netDisconnect = document.getElementById("net-disconnect") as HTMLButtonElement;
const netDownload = document.getElementById("net-download") as HTMLAnchorElement;
const netDownloadLabel = document.getElementById("net-download-label")!;
const netPlatforms = document.getElementById("net-platforms")!;
const netRun = document.getElementById("net-run")!;
const netRunHint = document.getElementById("net-run-hint")!;
const netCopy = document.getElementById("net-copy") as HTMLButtonElement;
const netShell = document.getElementById("net-shell")!;
let netState: NetState = "off";

type Platform = { id: string; label: string; chip: string };
const platforms: Platform[] = [
  { id: "darwin-arm64", label: "macOS (Apple silicon)", chip: "macOS · Apple silicon" },
  { id: "darwin-amd64", label: "macOS (Intel)", chip: "macOS · Intel" },
  { id: "linux-amd64", label: "Linux (x86-64)", chip: "Linux · x86-64" },
  { id: "linux-arm64", label: "Linux (ARM64)", chip: "Linux · ARM64" },
  { id: "windows-amd64", label: "Windows", chip: "Windows" },
];

function detectPlatform(): string {
  const ua = navigator.userAgent;
  const hint = (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData?.platform ?? "";
  if (/win/i.test(hint) || /Windows/.test(ua)) return "windows-amd64";
  if (/mac/i.test(hint) || /Mac OS X|Macintosh/.test(ua)) return "darwin-arm64";
  return /aarch64|arm64/i.test(ua) ? "linux-arm64" : "linux-amd64";
}

// The commands to run a downloaded relay, without prompts (that is what Copy
// copies), and a hint shown under them.
function runInstructions(id: string): { lines: string[]; hint: string } {
  const file = `bootlet-relay-${id}${id.startsWith("windows") ? ".exe" : ""}`;
  if (id.startsWith("windows")) {
    return {
      lines: ["cd $HOME\\Downloads", `.\\${file}`],
      hint: "PowerShell, or double-click the file. If SmartScreen warns, choose More info → Run anyway.",
    };
  }
  if (id.startsWith("darwin")) {
    return {
      lines: ["cd ~/Downloads", `chmod +x ${file}`, `xattr -c ${file}`, `./${file}`],
      hint: "xattr clears the download quarantine, so macOS lets the unsigned binary run.",
    };
  }
  return { lines: ["cd ~/Downloads", `chmod +x ${file}`, `./${file}`], hint: "Keep it running while you use bootlet." };
}

let platform = detectPlatform();
function selectPlatform(id: string) {
  platform = id;
  const p = platforms.find((x) => x.id === id)!;
  netDownload.href = RELEASES + id + (id.startsWith("windows") ? ".exe" : "");
  netDownloadLabel.textContent = `Download for ${p.label}`;
  for (const chip of netPlatforms.children) chip.setAttribute("aria-checked", String((chip as HTMLElement).dataset.id === id));
  const { lines, hint } = runInstructions(id);
  const prompt = id.startsWith("windows") ? "PS> " : "$ ";
  netShell.textContent = id.startsWith("windows") ? "PowerShell" : "Terminal";
  netRun.replaceChildren(
    ...lines.flatMap((line, i) => [
      Object.assign(document.createElement("span"), { className: "prompt", textContent: prompt }),
      document.createTextNode(line + (i < lines.length - 1 ? "\n" : "")),
    ]),
  );
  netRunHint.textContent = hint;
}
netPlatforms.append(
  ...platforms.map((p) => {
    const b = Object.assign(document.createElement("button"), { type: "button", className: "chip", textContent: p.chip });
    b.dataset.id = p.id;
    b.setAttribute("role", "radio");
    b.onclick = () => selectPlatform(p.id);
    return b;
  }),
);
selectPlatform(platform);

netCopy.onclick = async () => {
  try {
    await navigator.clipboard.writeText(runInstructions(platform).lines.join("\n"));
    netCopy.dataset.copied = "";
    netCopy.querySelector("span")!.textContent = "Copied";
    setTimeout(() => {
      delete netCopy.dataset.copied;
      netCopy.querySelector("span")!.textContent = "Copy";
    }, 1500);
  } catch {
    // clipboard unavailable (insecure context): the text is still selectable
  }
};

function renderNetDialog() {
  const statusText: Record<NetState, string> = {
    off: "Not connected. The guest is offline.",
    connecting: `Connecting to ${relay}…`,
    online: servedByRelay ? "Online through the relay serving this page" : `Online through ${relay}`,
    offline: `Can't reach ${relay}. Is the relay running? Retrying…`,
  };
  netStatus.dataset.state = netState;
  netStatusText.textContent = statusText[netState];
  netSetup.hidden = netState === "online";
  netOnline.hidden = netState !== "online";
  netDisconnect.hidden = netState === "off" || servedByRelay;
  netConnect.disabled = netState === "connecting";
  netConnect.textContent = netState === "connecting" ? "Connecting…" : "Connect";
}

netEl.onclick = () => {
  netURL.value = relay || LOCAL_RELAY;
  renderNetDialog();
  netDialog.showModal();
};
netConnect.onclick = () => {
  relay = netURL.value.trim() || LOCAL_RELAY;
  send({ type: "relay", url: relay });
};
netURL.onkeydown = (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    netConnect.click();
  }
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
