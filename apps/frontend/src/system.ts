// The System tab: live stats of the guest (CPU, memory, storage, network,
// top processes) from bootlet-agent, and of the emulator (speed, memory,
// browser storage). The guest is polled every 2 s, only while the tab is on
// screen: a poll costs the guest about 1.7M instructions (a few percent of
// its CPU), and an idle machine stays idle otherwise.
import type { EmulatorStats, GuestStats } from "@bootlet/shared";
import type { AgentClient } from "./agent";

const HISTORY = 60; // samples in each sparkline
const POLL_MS = 2000;

export function formatBytes(n: number) {
  if (n < 1024) return `${Math.round(n)} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let i = -1;
  do {
    n /= 1024;
    i++;
  } while (n >= 1024 && i < units.length - 1);
  return `${n < 10 ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
}

const pct = (x: number) => `${Math.round(x)}%`;

function formatUptime(s: number) {
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return m ? `${m}m ${Math.floor(s % 60)}s` : `${Math.floor(s)}s`;
}

const el = <K extends keyof HTMLElementTagNameMap>(tag: K, cls = "", text = "") => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text) e.textContent = text;
  return e;
};

/**
 * A line chart of the last HISTORY samples, taken every step seconds, one
 * or more series on one axis, with a hover readout. max: a fixed top (100
 * for percentages) or 0 to scale to the largest value.
 */
function sparkline(series: string[], max: number, step: number, format: (v: number[]) => string) {
  const wrap = el("div", "spark");
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", `0 0 ${HISTORY - 1} 40`);
  svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("aria-hidden", "true");
  const lines = series.map((s) => {
    const l = document.createElementNS(ns, "polyline");
    l.setAttribute("class", s);
    svg.append(l);
    return l;
  });
  const cursor = el("div", "spark-cursor");
  const tip = el("div", "spark-tip");
  cursor.hidden = tip.hidden = true;
  wrap.append(svg, cursor, tip);
  const data: number[][] = series.map(() => []);

  let hover = -1;
  const showTip = () => {
    const n = data[0].length;
    if (hover < 0 || n === 0) {
      cursor.hidden = tip.hidden = true;
      return;
    }
    // Samples are right-aligned: the newest is at the right edge.
    const i = Math.min(n - 1, Math.max(0, hover - (HISTORY - n)));
    const x = ((HISTORY - n + i) / (HISTORY - 1)) * 100;
    const ago = (n - 1 - i) * step;
    tip.textContent = `${ago ? `${ago}s ago` : "now"} · ${format(data.map((d) => d[i]))}`;
    cursor.style.left = `${x}%`;
    tip.style.left = `${Math.min(Math.max(x, 18), 82)}%`;
    cursor.hidden = tip.hidden = false;
  };
  wrap.onpointermove = (e) => {
    const r = wrap.getBoundingClientRect();
    hover = Math.round(((e.clientX - r.left) / r.width) * (HISTORY - 1));
    showTip();
  };
  wrap.onpointerleave = () => {
    hover = -1;
    showTip();
  };

  return {
    el: wrap,
    push(values: number[]) {
      values.forEach((v, s) => {
        data[s].push(v);
        if (data[s].length > HISTORY) data[s].shift();
      });
      const top = max || Math.max(1, ...data.flat()) * 1.15;
      lines.forEach((l, s) => {
        const d = data[s];
        l.setAttribute(
          "points",
          d.map((v, i) => `${HISTORY - d.length + i},${(40 - (Math.min(v, top) / top) * 38 - 1).toFixed(2)}`).join(" "),
        );
      });
      showTip();
    },
    clear() {
      data.forEach((d) => (d.length = 0));
      lines.forEach((l) => l.setAttribute("points", ""));
      showTip();
    },
  };
}

/** A horizontal bar of segments (fractions of the whole) with a hover title each. */
function stackBar(series: string[]) {
  const bar = el("div", "stack");
  const segs = series.map((s) => {
    const seg = el("span", s);
    bar.append(seg);
    return seg;
  });
  return {
    el: bar,
    set(fractions: number[], titles: string[]) {
      fractions.forEach((f, i) => {
        segs[i].style.flexBasis = `${Math.max(0, Math.min(1, f)) * 100}%`;
        segs[i].hidden = f <= 0;
        segs[i].title = titles[i];
      });
    },
  };
}

/** A legend item: swatch, label and a value that updates. */
function legendItem(series: string, label: string) {
  const li = el("li");
  const value = el("b");
  li.append(el("i", `sw ${series}`), `${label} `, value);
  return { el: li, value };
}

function card(title: string) {
  const sec = el("section", "card");
  sec.dataset.card = title;
  const head = el("div", "card-head");
  const value = el("span", "card-value");
  head.append(el("h3", "", title), value);
  sec.append(head);
  return { sec, value };
}

export interface SystemView {
  /** Re-evaluates whether to poll (panel open, tab shown, guest ready). */
  update(): void;
  /** Emulator stats from the worker, once a second. */
  emulator(s: EmulatorStats): void;
}

export function initSystem(client: AgentClient, root: HTMLElement, visible: () => boolean, note: () => string): SystemView {
  const meta = el("p", "sys-meta");
  const msg = el("p", "sys-msg");

  // CPU
  const cpu = card("CPU");
  const cpuSpark = sparkline(["s1"], 100, POLL_MS / 1000, ([v]) => pct(v));
  const cpuBar = stackBar(["s1", "s2", "s3"]);
  const cpuLegend = [legendItem("s1", "User"), legendItem("s2", "System"), legendItem("s3", "I/O wait")];
  const cpuList = el("ul", "legend");
  cpuList.append(...cpuLegend.map((l) => l.el));
  cpu.sec.append(cpuSpark.el, cpuBar.el, cpuList);

  // Memory
  const mem = card("Memory");
  const memBar = stackBar(["s1", "s3"]);
  const memLegend = [legendItem("s1", "Used"), legendItem("s3", "Cache"), legendItem("free", "Free")];
  const memList = el("ul", "legend");
  memList.append(...memLegend.map((l) => l.el));
  mem.sec.append(memBar.el, memList);

  // Storage
  const disk = card("Storage");
  const diskRows = el("div", "fs-rows");
  disk.sec.append(diskRows);

  // Network
  const net = card("Network");
  const netSpark = sparkline(["s1", "s2"], 0, POLL_MS / 1000, ([rx, tx]) => `↓ ${formatBytes(rx)}/s ↑ ${formatBytes(tx)}/s`);
  const netLegend = [legendItem("s1", "Download"), legendItem("s2", "Upload")];
  const netList = el("ul", "legend");
  netList.append(...netLegend.map((l) => l.el));
  net.sec.append(netSpark.el, netList);

  // Processes
  const procs = card("Top processes");
  const table = el("table", "procs");
  table.innerHTML = '<thead><tr><th>PID</th><th>Name</th><th>CPU</th><th>Memory</th></tr></thead>';
  const tbody = el("tbody");
  table.append(tbody);
  procs.sec.append(table);

  // Emulator (always available, even before the guest is up)
  const emu = card("Emulator");
  const emuSpark = sparkline(["s1"], 0, 1, ([v]) => `${v.toFixed(1)} MIPS`);
  const emuRows = el("dl", "kv");
  const emuActive = el("dd");
  const emuFps = el("dd");
  const emuEngine = el("dd");
  const emuMem = el("dd");
  const emuStore = el("dd", "", "—");
  emuRows.append(
    el("dt", "", "While executing"),
    emuActive,
    el("dt", "", "Display"),
    emuFps,
    el("dt", "", "Engine"),
    emuEngine,
    el("dt", "", "Tab memory"),
    emuMem,
    el("dt", "", "Browser storage"),
    emuStore,
  );
  emu.sec.append(emuSpark.el, emuRows);
  emu.value.title = "Million guest instructions per wall-clock second (idle time included)";
  emuActive.title = "Million guest instructions per second spent executing (excludes idle time)";

  const guestCards = [cpu.sec, mem.sec, disk.sec, net.sec, procs.sec];
  root.append(meta, msg, ...guestCards, emu.sec);

  let prev: { s: GuestStats } | undefined;
  let polling = false; // the poll loop is running

  function render(s: GuestStats) {
    const p = prev?.s;
    const dt = p ? s.uptime - p.uptime : 0;
    meta.textContent = `Up ${formatUptime(s.uptime)} · ${s.procs.length} processes (${s.tasks} tasks) · load ${s.load.map((l) => l.toFixed(2)).join(" ")}`;

    // CPU: shares of the time since the last snapshot.
    if (p && dt > 0) {
      const d = {
        user: s.cpu.user - p.cpu.user,
        system: s.cpu.system - p.cpu.system,
        iowait: s.cpu.iowait - p.cpu.iowait,
        idle: s.cpu.idle - p.cpu.idle,
      };
      const total = Math.max(1, d.user + d.system + d.iowait + d.idle);
      const f = [d.user / total, d.system / total, d.iowait / total];
      const busy = (f[0] + f[1]) * 100;
      cpu.value.textContent = pct(busy);
      cpuSpark.push([busy]);
      cpuBar.set(f, [`User ${pct(f[0] * 100)}`, `System ${pct(f[1] * 100)}`, `I/O wait ${pct(f[2] * 100)}`]);
      cpuLegend.forEach((l, i) => (l.value.textContent = pct(f[i] * 100)));
    }

    // Memory: used is what can't be reclaimed; cache can be.
    const m = s.mem;
    const used = m.total - m.available;
    const cache = Math.max(0, m.available - m.free);
    mem.value.textContent = `${formatBytes(used)} of ${formatBytes(m.total)}`;
    memBar.set([used / m.total, cache / m.total], [`Used ${formatBytes(used)}`, `Cache ${formatBytes(cache)}`]);
    memLegend[0].value.textContent = formatBytes(used);
    memLegend[1].value.textContent = formatBytes(cache);
    memLegend[2].value.textContent = formatBytes(m.free);

    // Storage
    const fss = s.fs ?? [];
    disk.value.textContent = "";
    diskRows.replaceChildren(
      ...fss.map((fs) => {
        const label =
          fs.mount === "/" ? (fs.type === "overlay" ? "Persistent disk" : "Root (in memory, not saved)") : `${fs.mount} (in memory)`;
        const row = el("div", "fs-row");
        const head = el("div", "fs-head");
        head.append(el("span", "", label), el("span", "fs-val", `${formatBytes(fs.used)} of ${formatBytes(fs.total)}`));
        const bar = stackBar(["s1"]);
        bar.set([fs.used / fs.total], [`${pct((fs.used / fs.total) * 100)} used, ${formatBytes(fs.free)} free`]);
        row.append(head, bar.el);
        return row;
      }),
    );
    if (fss.length === 0) diskRows.append(el("p", "hint", "No filesystem sizes available."));

    // Network
    net.sec.hidden = !s.net;
    if (s.net && p?.net && dt > 0) {
      const rx = Math.max(0, (s.net.rx - p.net.rx) / dt);
      const tx = Math.max(0, (s.net.tx - p.net.tx) / dt);
      netSpark.push([rx, tx]);
      netLegend[0].value.textContent = `${formatBytes(rx)}/s`;
      netLegend[1].value.textContent = `${formatBytes(tx)}/s`;
      net.value.textContent = `${formatBytes(s.net.rx)} in · ${formatBytes(s.net.tx)} out`;
    }

    // Processes: CPU share since the last snapshot, then memory.
    const before = new Map(p?.procs.map((x) => [x.pid, x.ticks]) ?? []);
    const rows = s.procs
      .map((x) => ({ ...x, cpu: dt > 0 && before.has(x.pid) ? ((x.ticks - before.get(x.pid)!) / s.ticks / dt) * 100 : 0 }))
      .filter((x) => x.rss > 0 || x.cpu >= 1)
      .sort((a, b) => b.cpu - a.cpu || b.rss - a.rss)
      .slice(0, 6);
    tbody.replaceChildren(
      ...rows.map((x) => {
        const tr = el("tr");
        tr.append(el("td", "num", String(x.pid)), el("td", "pname", x.name), el("td", "num", pct(x.cpu)), el("td", "num", x.rss ? formatBytes(x.rss) : "—"));
        return tr;
      }),
    );
  }

  // One request at a time, a second apart, for as long as the tab is shown.
  async function poll() {
    if (polling) return;
    polling = true;
    void estimate();
    while (active()) {
      try {
        const s = await client.stats();
        if (active()) {
          render(s);
          prev = { s };
        }
      } catch {
        // the guest stopped or rebooted: update() resets the view
      }
      await new Promise((r) => setTimeout(r, POLL_MS));
    }
    polling = false;
  }

  const active = () => visible() && client.ready && document.visibilityState === "visible";

  async function estimate() {
    try {
      const e = await navigator.storage?.estimate?.();
      if (e?.usage !== undefined) emuStore.textContent = formatBytes(e.usage);
    } catch {
      // not available
    }
  }

  function update() {
    const ready = client.ready;
    msg.textContent = ready ? "" : note();
    msg.hidden = ready;
    for (const c of guestCards) c.hidden = !ready || (c === net.sec && prev !== undefined && !prev.s.net);
    meta.hidden = !ready;
    if (!ready) {
      prev = undefined;
      cpuSpark.clear();
      netSpark.clear();
      cpu.value.textContent = "";
    }
    if (active()) void poll();
  }
  document.addEventListener("visibilitychange", update);
  update();

  return {
    update,
    emulator({ mips, activeMips, fps, memMiB, engine }) {
      const fmt = (v: number) => (v < 10 ? v.toFixed(1) : String(Math.round(v)));
      emuSpark.push([mips]);
      emu.value.textContent = `${fmt(mips)} MIPS`;
      emuActive.textContent = activeMips > 0 ? `${fmt(activeMips)} MIPS` : "idle";
      emuFps.textContent = `${fmt(fps)} frames/s`;
      emuEngine.textContent =
        engine.engine === "jit"
          ? `WebAssembly JIT · ${engine.compiled.toLocaleString()} blocks compiled`
          : engine.engine === "blocks"
            ? "Decoded-block cache"
            : "Interpreter";
      emuEngine.title =
        engine.engine === "jit" ? `${engine.codeKiB.toLocaleString()} KiB generated in ${Math.round(engine.compileMs)} ms` : "";
      emuMem.textContent = `${memMiB} MiB`;
      if (visible()) void estimate();
    },
  };
}
