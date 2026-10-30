import "@xterm/xterm/css/xterm.css";
import "./style.css";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { shellPrompt, type FromWorker, type ToWorker, type VMState } from "@bootlet/shared";

const statusEl = document.getElementById("status")!;
const statusText = document.getElementById("status-text")!;
const toggleBtn = document.getElementById("toggle") as HTMLButtonElement;
const restartBtn = document.getElementById("restart") as HTMLButtonElement;

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
      statusEl.title =
        msg.backend === "none"
          ? `/data is not persistent: ${msg.detail ?? "browser storage unavailable"}`
          : `/data is saved in this browser (${msg.backend === "opfs" ? "Origin Private File System" : "IndexedDB"})`;
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
    send({ type: "start", base: document.baseURI });
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

render();
send({ type: "start", base: document.baseURI });
term.focus();
