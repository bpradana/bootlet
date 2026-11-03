// The side panel. Its Files tab browses the guest's filesystem through
// bootlet-agent: files and folders dropped from the computer are uploaded;
// items dragged onto a folder (or a crumb) are moved, or copied with
// Alt/Option; in Chrome and Edge a file can be dragged out to the desktop.
// Its System tab shows live stats (system.ts).
import type { AgentEntry, EmulatorStats } from "@bootlet/shared";
import { AgentClient, AgentError } from "./agent";
import { initSystem } from "./system";

/** Drag data type for guest paths (a JSON array), also accepted by the terminal. */
export const pathsType = "application/x-bootlet-paths";

const WIDTH_KEY = "bootlet.files.width";
const OPEN_KEY = "bootlet.files.open";
const TAB_KEY = "bootlet.panel.tab";
const DRAG_OUT_MAX = 8 << 20; // files prefetched so they can be dragged out

const $ = <T extends HTMLElement = HTMLElement>(id: string) => document.getElementById(id) as T;

export const join = (dir: string, name: string) => (dir === "/" ? `/${name}` : `${dir}/${name}`);
const parent = (p: string) => p.slice(0, p.lastIndexOf("/")) || "/";
const base = (p: string) => p.slice(p.lastIndexOf("/") + 1);

/** Quotes a path for the shell when it needs it. */
export const shellQuote = (p: string) => (/^[\w@%+=:,./-]+$/.test(p) ? p : `'${p.replaceAll("'", `'\\''`)}'`);

const store = {
  get(key: string) {
    try {
      return localStorage.getItem(key);
    } catch {
      return null;
    }
  },
  set(key: string, value: string) {
    try {
      localStorage.setItem(key, value);
    } catch {
      // storage unavailable: not remembered
    }
  },
};

function formatSize(n: number) {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let i = -1;
  do {
    n /= 1024;
    i++;
  } while (n >= 1024 && i < units.length - 1);
  return `${n < 10 ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
}

const timeFmt = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" });
const dayFmt = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" });
const yearFmt = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric" });
const fullFmt = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

function formatDate(ms: number) {
  if (ms <= 0) return "—"; // files from the initramfs carry no date
  const d = new Date(ms);
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return timeFmt.format(d);
  return d.getFullYear() === now.getFullYear() ? dayFmt.format(d) : yearFmt.format(d);
}

function modeString(e: AgentEntry) {
  const t = e.type === "dir" ? "d" : e.type === "link" ? "l" : "-";
  let s = t;
  for (let i = 8; i >= 0; i--) s += e.mode & (1 << i) ? "rwx"[(8 - i) % 3] : "-";
  return s;
}

const icons = {
  dir: '<path d="M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2z" />',
  file: '<path d="M14 3H7a2 2 0 00-2 2v14a2 2 0 002 2h10a2 2 0 002-2V8z" /><path d="M14 3v5h5" />',
  link: '<path d="M10 14a4 4 0 005.66 0l3-3a4 4 0 00-5.66-5.66l-1 1M14 10a4 4 0 00-5.66 0l-3 3a4 4 0 005.66 5.66l1-1" />',
  other: '<circle cx="12" cy="12" r="8" /><path d="M12 8v4M12 16h.01" />',
};
const svg = (paths: string) => `<svg viewBox="0 0 24 24" aria-hidden="true">${paths}</svg>`;

const isDir = (e: AgentEntry) => e.type === "dir" || (e.type === "link" && !!e.targetDir);

type Conflict = "replace" | "keep" | "skip";

/** An upload source: a file, or a folder (relative paths of its contents). */
type Upload = { name: string; file?: File; dirs: string[]; files: { path: string; file: File }[] };

export interface FilesOptions {
  /** Sends bytes to the guest's file agent. */
  send(data: Uint8Array<ArrayBuffer>): void;
  /** Types text at the terminal (paths). */
  type(text: string): void;
}

export function initFiles(opts: FilesOptions) {
  const client = new AgentClient(opts.send);

  const panel = $("files");
  const splitter = $("files-splitter");
  const toggleBtn = $<HTMLButtonElement>("files-toggle");
  const list = $("files-list");
  const crumbs = $("files-crumbs");
  const msg = $("files-msg");
  const drop = $("files-drop");
  const dropText = $("files-drop-text");
  const transfers = $("files-transfers");
  const hint = $("files-hint");
  const input = $<HTMLInputElement>("files-input");
  const menu = $("files-menu");
  const conflictDlg = $<HTMLDialogElement>("files-conflict");
  const tools = ["files-up", "files-home", "files-refresh", "files-mkdir", "files-upload"].map((id) => $<HTMLButtonElement>(id));

  let cwd = "";
  let entries: AgentEntry[] = [];
  let selected = new Set<string>();
  let anchor = -1; // index for Shift-click ranges
  let error = "";
  let loading = false;
  let vmNote = "Waiting for the guest to boot…";

  // ---- tabs ----

  type Tab = "files" | "system";
  const tabs: Record<Tab, { button: HTMLButtonElement; view: HTMLElement }> = {
    files: { button: $<HTMLButtonElement>("tab-files"), view: $("files-view") },
    system: { button: $<HTMLButtonElement>("tab-system"), view: $("sys-view") },
  };
  let tab: Tab = store.get(TAB_KEY) === "system" ? "system" : "files";
  const system = initSystem(
    client,
    tabs.system.view,
    () => !panel.hidden && tab === "system",
    () => vmNote,
  );

  function showTab(t: Tab, focus = false) {
    tab = t;
    store.set(TAB_KEY, t);
    for (const [name, { button, view }] of Object.entries(tabs) as [Tab, (typeof tabs)[Tab]][]) {
      const on = name === t;
      button.setAttribute("aria-selected", String(on));
      button.tabIndex = on ? 0 : -1;
      view.hidden = !on;
    }
    $("files-tools").hidden = t !== "files";
    if (t === "files" && client.ready && !cwd && !panel.hidden) void navigate(client.home);
    if (focus) tabs[t].button.focus();
    system.update();
  }
  tabs.files.button.onclick = () => showTab("files");
  tabs.system.button.onclick = () => showTab("system");
  for (const { button } of Object.values(tabs)) {
    button.onkeydown = (e) => {
      if (e.key === "ArrowLeft" || e.key === "ArrowRight") showTab(tab === "files" ? "system" : "files", true);
    };
  }

  // ---- open / close / resize ----

  const setWidth = (w: number) => {
    const max = Math.max(260, window.innerWidth - 320);
    panel.style.setProperty("--files-w", `${Math.round(Math.min(Math.max(w, 240), max))}px`);
  };
  setWidth(Number(store.get(WIDTH_KEY)) || 380);

  function setOpen(open: boolean) {
    panel.hidden = !open;
    splitter.hidden = !open;
    toggleBtn.setAttribute("aria-pressed", String(open));
    store.set(OPEN_KEY, open ? "1" : "");
    if (open && tab === "files") {
      if (client.ready && !cwd) void navigate(client.home);
      list.focus();
    }
    system.update();
  }
  toggleBtn.onclick = () => setOpen(!!panel.hidden);
  $("files-close").onclick = () => setOpen(false);
  showTab(tab);
  if (store.get(OPEN_KEY)) setOpen(true);

  splitter.onpointerdown = (e) => {
    splitter.setPointerCapture(e.pointerId);
    const right = panel.getBoundingClientRect().right;
    splitter.onpointermove = (ev) => setWidth(right - ev.clientX);
    splitter.onpointerup = () => {
      splitter.onpointermove = splitter.onpointerup = null;
      store.set(WIDTH_KEY, String(panel.getBoundingClientRect().width));
    };
  };
  splitter.onkeydown = (e) => {
    const step = e.key === "ArrowLeft" ? 24 : e.key === "ArrowRight" ? -24 : 0;
    if (!step) return;
    setWidth(panel.getBoundingClientRect().width + step);
    store.set(WIDTH_KEY, String(panel.getBoundingClientRect().width));
  };

  // ---- rendering ----

  function render() {
    const ready = client.ready;
    for (const b of tools) b.disabled = !ready || !cwd;
    $<HTMLButtonElement>("files-up").disabled = !ready || cwd === "/" || !cwd;

    crumbs.replaceChildren();
    if (cwd) {
      const parts = cwd === "/" ? [""] : cwd.split("/");
      parts.forEach((part, i) => {
        const path = parts.slice(0, i + 1).join("/") || "/";
        if (i > 0) crumbs.append(Object.assign(document.createElement("span"), { className: "sep", textContent: "›" }));
        const b = Object.assign(document.createElement("button"), { type: "button", className: "crumb", textContent: part || "/" });
        b.title = path;
        b.onclick = () => void navigate(path);
        dropTarget(b, path);
        crumbs.append(b);
      });
      crumbs.scrollLeft = crumbs.scrollWidth;
    }

    const note = !ready ? vmNote : error || (loading && entries.length === 0 ? "Loading…" : entries.length === 0 ? "This folder is empty." : "");
    msg.textContent = note;
    msg.hidden = !note;
    msg.classList.toggle("error", ready && !!error);
    hint.hidden = !ready;

    list.replaceChildren(
      ...(ready && !error
        ? entries.map((e, i) => {
            const row = document.createElement("div");
            row.className = "row";
            row.dataset.type = isDir(e) ? "dir" : e.type;
            row.setAttribute("role", "option");
            row.setAttribute("aria-selected", String(selected.has(e.name)));
            row.draggable = true;
            row.title = `${e.name}\n${modeString(e)}  ${e.type === "dir" ? "" : formatSize(e.size) + "  "}${fullFmt.format(e.mtime)}${e.target ? `\n→ ${e.target}` : ""}`;
            row.innerHTML = `${svg(icons[e.type])}<span class="name"></span><span class="size">${e.type === "file" ? formatSize(e.size) : ""}</span><span class="date">${formatDate(e.mtime)}</span>`;
            row.querySelector(".name")!.textContent = e.name;
            row.onclick = (ev) => select(i, ev);
            row.ondblclick = () => void open(e);
            row.oncontextmenu = (ev) => {
              ev.preventDefault();
              if (!selected.has(e.name)) select(i);
              showMenu(ev.clientX, ev.clientY, e);
            };
            row.onpointerenter = () => schedulePrefetch(e);
            row.onpointerleave = () => clearTimeout(prefetchTimer);
            row.ondragstart = (ev) => dragStart(ev, e, i);
            if (isDir(e)) dropTarget(row, join(cwd, e.name));
            return row;
          })
        : []),
    );
  }

  /** Updates the selection on the existing rows (re-rendering would break double-clicks). */
  function paintSelection() {
    [...list.children].forEach((row, i) => row.setAttribute("aria-selected", String(selected.has(entries[i]?.name))));
  }

  function select(i: number, ev?: MouseEvent) {
    const name = entries[i].name;
    if (ev?.shiftKey && anchor >= 0) {
      const [a, b] = [Math.min(anchor, i), Math.max(anchor, i)];
      selected = new Set(entries.slice(a, b + 1).map((e) => e.name));
    } else if (ev && (ev.metaKey || ev.ctrlKey)) {
      if (selected.has(name)) selected.delete(name);
      else selected.add(name);
      anchor = i;
    } else {
      selected = new Set([name]);
      anchor = i;
    }
    paintSelection();
    list.children[i]?.scrollIntoView({ block: "nearest" });
    const e = entries[i];
    if (selected.size === 1 && e.type === "file") prefetch(join(cwd, e.name), e);
  }

  const selectedPaths = () => entries.filter((e) => selected.has(e.name)).map((e) => join(cwd, e.name));

  // ---- navigation ----

  let loadSeq = 0;
  async function navigate(path: string, keep = false) {
    if (!client.ready) return;
    const seq = ++loadSeq;
    if (path !== cwd) {
      entries = [];
      selected.clear();
      anchor = -1;
    }
    cwd = path;
    loading = true;
    error = "";
    render();
    try {
      const got = await client.list(path);
      if (seq !== loadSeq) return;
      entries = got;
      if (keep) selected = new Set([...selected].filter((n) => got.some((e) => e.name === n)));
    } catch (e) {
      if (seq !== loadSeq) return;
      entries = [];
      error = e instanceof AgentError && e.code === "notexist" ? `${path} no longer exists.` : `Can't open ${path}: ${(e as Error).message}`;
    }
    loading = false;
    render();
  }
  const refresh = () => navigate(cwd, true);

  async function open(e: AgentEntry) {
    if (isDir(e)) return navigate(join(cwd, e.name));
    if (e.type === "file") return download([join(cwd, e.name)]);
  }

  $("files-up").onclick = () => void navigate(parent(cwd));
  $("files-home").onclick = () => void navigate(client.home);
  $("files-refresh").onclick = () => void refresh();

  // ---- errors ----

  function toast(text: string) {
    const li = document.createElement("li");
    li.className = "transfer failed";
    li.innerHTML = `<span class="t-name"></span><button type="button" class="tool" aria-label="Dismiss">${svg('<path d="M6 6l12 12M18 6L6 18" />')}</button>`;
    li.querySelector(".t-name")!.textContent = text;
    li.querySelector("button")!.onclick = () => {
      li.remove();
      transfers.hidden = transfers.children.length === 0;
    };
    transfers.append(li);
    transfers.hidden = false;
  }
  const message = (e: unknown) => (e instanceof Error ? e.message : String(e));

  // ---- transfers ----

  /** A row in the transfer list with a progress bar and a cancel button. */
  function transfer(label: string, total: number) {
    const ctl = new AbortController();
    const li = document.createElement("li");
    li.className = "transfer";
    li.innerHTML = `<span class="t-name"></span><span class="t-size"></span><button type="button" class="tool" aria-label="Cancel">${svg('<path d="M6 6l12 12M18 6L6 18" />')}</button><progress max="1" value="0"></progress>`;
    li.querySelector(".t-name")!.textContent = label;
    const size = li.querySelector(".t-size")!;
    const bar = li.querySelector("progress")!;
    li.querySelector("button")!.onclick = () => ctl.abort();
    transfers.append(li);
    transfers.hidden = false;
    const done = () => {
      li.remove();
      transfers.hidden = transfers.children.length === 0;
    };
    return {
      signal: ctl.signal,
      progress(n: number) {
        bar.value = total ? n / total : 1;
        size.textContent = total ? `${formatSize(n)} / ${formatSize(total)}` : "";
      },
      label(text: string) {
        li.querySelector(".t-name")!.textContent = text;
      },
      finish(err?: unknown) {
        if (err === undefined || ctl.signal.aborted) return done();
        done();
        toast(`${label}: ${message(err)}`);
      },
    };
  }

  // Transfers run one at a time, in order.
  let queue: Promise<unknown> = Promise.resolve();
  const enqueue = <T>(job: () => Promise<T>) => (queue = queue.then(job, job));

  async function download(paths: string[]) {
    for (const path of paths) {
      let e = entries.find((x) => join(cwd, x.name) === path);
      try {
        e ??= await client.stat(path);
      } catch (err) {
        toast(`${base(path)}: ${message(err)}`);
        continue;
      }
      if (e.type !== "file") continue;
      const file = e;
      void enqueue(async () => {
        const t = transfer(`Downloading ${file.name}`, file.size);
        try {
          const blob = cached.get(path)?.blob ?? (await client.download(path, file.size, t.progress, t.signal));
          const a = Object.assign(document.createElement("a"), { href: URL.createObjectURL(blob), download: file.name });
          a.click();
          setTimeout(() => URL.revokeObjectURL(a.href), 10_000);
          t.finish();
        } catch (err) {
          t.finish(err);
        }
      });
    }
  }

  // Conflicts: ask once per item, or once for all with the checkbox.
  async function askConflict(name: string, where: string, remembered: { choice?: Conflict }): Promise<Conflict> {
    if (remembered.choice) return remembered.choice;
    $("conflict-title").textContent = `Replace “${name}”?`;
    $("conflict-text").textContent = `An item named “${name}” already exists in ${where}.`;
    const all = $<HTMLInputElement>("conflict-all");
    all.checked = false;
    conflictDlg.returnValue = "";
    conflictDlg.showModal();
    await new Promise((r) => conflictDlg.addEventListener("close", r, { once: true }));
    const choice = (conflictDlg.returnValue || "skip") as Conflict;
    if (all.checked) remembered.choice = choice;
    return choice;
  }

  /** A name like "name (1).ext" that doesn't exist in dir. */
  async function freeName(dir: string, name: string) {
    const dot = name.lastIndexOf(".");
    const [stem, ext] = dot > 0 ? [name.slice(0, dot), name.slice(dot)] : [name, ""];
    for (let i = 1; ; i++) {
      const n = `${stem} (${i})${ext}`;
      if (!(await client.exists(join(dir, n)))) return n;
    }
  }

  /** Resolves a name clash in dir: the name to use, or undefined to skip. */
  async function resolveName(dir: string, name: string, remembered: { choice?: Conflict }, isDirectory = false) {
    if (!(await client.exists(join(dir, name)))) return { name, overwrite: false };
    const choice = await askConflict(name, dir, remembered);
    if (choice === "skip") return undefined;
    if (choice === "keep") return { name: await freeName(dir, name), overwrite: false };
    if (isDirectory) await client.op("remove", join(dir, name)); // replace a folder: start fresh
    return { name, overwrite: true };
  }

  function upload(items: Upload[], dir: string) {
    const remembered: { choice?: Conflict } = {};
    for (const item of items) {
      void enqueue(async () => {
        const files = item.file ? [{ path: "", file: item.file }] : item.files;
        const total = files.reduce((n, f) => n + f.file.size, 0);
        const t = transfer(`Uploading ${item.name}`, total);
        try {
          const target = await resolveName(dir, item.name, remembered, !item.file);
          if (!target) return t.finish();
          const root = join(dir, target.name);
          let done = 0;
          if (!item.file) {
            await client.op("mkdir", root);
            for (const d of item.dirs) await client.op("mkdir", join(root, d));
          }
          for (const [i, f] of files.entries()) {
            if (!item.file) t.label(`Uploading ${item.name} (${i + 1}/${files.length})`);
            await client.upload(f.path ? join(root, f.path) : root, f.file, (n) => t.progress(done + n), t.signal);
            done += f.file.size;
          }
          t.finish();
        } catch (err) {
          t.finish(err);
        }
        if (dir === cwd) void refresh();
      });
    }
  }

  // Reads dropped items now (DataTransfer items are only valid during the
  // drop event), then walks folders.
  async function readDrop(dt: DataTransfer): Promise<Upload[]> {
    const fsEntries = [...dt.items]
      .filter((it) => it.kind === "file")
      .map((it) => it.webkitGetAsEntry?.() ?? null);
    const plain = [...dt.files];
    if (fsEntries.length === 0 || fsEntries.some((e) => e === null)) {
      return plain.map((f) => ({ name: f.name, file: f, dirs: [], files: [] }));
    }
    const fileOf = (e: FileSystemFileEntry) => new Promise<File>((res, rej) => e.file(res, rej));
    const readAll = async (d: FileSystemDirectoryEntry) => {
      const reader = d.createReader();
      const out: FileSystemEntry[] = [];
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((res, rej) => reader.readEntries(res, rej));
        if (batch.length === 0) return out;
        out.push(...batch);
      }
    };
    const items: Upload[] = [];
    for (const e of fsEntries as FileSystemEntry[]) {
      if (e.isFile) {
        items.push({ name: e.name, file: await fileOf(e as FileSystemFileEntry), dirs: [], files: [] });
        continue;
      }
      const item: Upload = { name: e.name, dirs: [], files: [] };
      const walk = async (d: FileSystemDirectoryEntry, rel: string) => {
        for (const c of await readAll(d)) {
          const p = rel ? `${rel}/${c.name}` : c.name;
          if (c.isDirectory) {
            item.dirs.push(p);
            await walk(c as FileSystemDirectoryEntry, p);
          } else {
            item.files.push({ path: p, file: await fileOf(c as FileSystemFileEntry) });
          }
        }
      };
      await walk(e as FileSystemDirectoryEntry, "");
      items.push(item);
    }
    return items;
  }

  $("files-upload").onclick = () => input.click();
  input.onchange = () => {
    upload([...(input.files ?? [])].map((f) => ({ name: f.name, file: f, dirs: [], files: [] })), cwd);
    input.value = "";
  };

  // ---- moving, copying, deleting, renaming ----

  async function moveInto(paths: string[], dir: string, copy: boolean) {
    const remembered: { choice?: Conflict } = {};
    for (const p of paths) {
      if (!copy && parent(p) === dir) continue;
      if (dir === p || dir.startsWith(p + "/")) {
        toast(`Can't ${copy ? "copy" : "move"} ${base(p)} into itself.`);
        continue;
      }
      try {
        const target = await resolveName(dir, base(p), remembered);
        if (target) await client.move(copy ? "copy" : "rename", p, join(dir, target.name), target.overwrite);
      } catch (e) {
        toast(`${copy ? "Copying" : "Moving"} ${base(p)}: ${message(e)}`);
      }
    }
    await refresh();
  }

  async function remove(paths: string[]) {
    if (paths.length === 0) return;
    const what = paths.length === 1 ? `“${base(paths[0])}”` : `${paths.length} items`;
    if (!confirm(`Delete ${what}? This can't be undone.`)) return;
    for (const p of paths) {
      try {
        await client.op("remove", p);
      } catch (e) {
        toast(`Deleting ${base(p)}: ${message(e)}`);
      }
    }
    selected.clear();
    await refresh();
  }

  /** Edits a row's name in place; resolves with the new name or undefined. */
  function editName(name: string): Promise<string | undefined> {
    const i = entries.findIndex((e) => e.name === name);
    const row = list.children[i] as HTMLElement | undefined;
    if (!row) return Promise.resolve(undefined);
    const field = Object.assign(document.createElement("input"), { className: "rename", value: name, spellcheck: false });
    row.querySelector(".name")!.replaceWith(field);
    row.draggable = false;
    field.focus();
    const dot = name.lastIndexOf(".");
    field.setSelectionRange(0, dot > 0 && entries[i].type === "file" ? dot : name.length);
    return new Promise((resolve) => {
      let finished = false;
      const finish = (v: string | undefined) => {
        if (finished) return;
        finished = true;
        resolve(v && v !== name && !v.includes("/") ? v : undefined);
        list.focus();
      };
      field.onkeydown = (e) => {
        e.stopPropagation();
        if (e.key === "Enter") finish(field.value.trim());
        if (e.key === "Escape") finish(undefined);
      };
      field.onblur = () => finish(field.value.trim());
      field.onclick = field.ondblclick = (e) => e.stopPropagation();
    });
  }

  async function rename(name: string) {
    const to = await editName(name);
    if (!to) return render();
    try {
      try {
        await client.move("rename", join(cwd, name), join(cwd, to));
      } catch (e) {
        if (!(e instanceof AgentError && e.code === "exist")) throw e;
        if ((await askConflict(to, cwd, {})) !== "replace") return render();
        await client.move("rename", join(cwd, name), join(cwd, to), true);
      }
      selected = new Set([to]);
    } catch (e) {
      toast(`Renaming ${name}: ${message(e)}`);
    }
    await refresh();
  }

  $("files-mkdir").onclick = async () => {
    try {
      let name = "New folder";
      if (await client.exists(join(cwd, name))) name = await freeName(cwd, name);
      await client.op("mkdir", join(cwd, name));
      selected = new Set([name]);
      await refresh();
      await rename(name);
    } catch (e) {
      toast(`New folder: ${message(e)}`);
    }
  };

  // ---- context menu ----

  function showMenu(x: number, y: number, e?: AgentEntry) {
    const paths = selectedPaths();
    const items: [string, () => void, string?][] = [];
    if (e) {
      if (isDir(e)) items.push(["Open", () => void navigate(join(cwd, e.name))]);
      const files = entries.filter((x) => selected.has(x.name) && x.type === "file");
      if (files.length) items.push([files.length > 1 ? `Download ${files.length} files` : "Download", () => void download(files.map((f) => join(cwd, f.name)))]);
      if (paths.length === 1) items.push(["Rename", () => void rename(e.name), "F2"]);
      items.push(["Copy path", () => void navigator.clipboard?.writeText(paths.join(" ")).catch(() => {})]);
      items.push(["Insert path in terminal", () => opts.type(paths.map(shellQuote).join(" ") + " ")]);
      items.push(["Delete", () => void remove(paths), "Del"]);
    } else {
      items.push(["New folder", () => $("files-mkdir").click()], ["Upload files…", () => input.click()], ["Refresh", () => void refresh()]);
      items.push(["Insert path in terminal", () => opts.type(shellQuote(cwd) + " ")]);
    }
    menu.replaceChildren(
      ...items.map(([label, fn, key]) => {
        const b = Object.assign(document.createElement("button"), { type: "button", className: label === "Delete" ? "danger" : "" });
        b.setAttribute("role", "menuitem");
        b.append(label);
        if (key) b.append(Object.assign(document.createElement("kbd"), { textContent: key }));
        b.onclick = () => {
          hideMenu();
          fn();
        };
        return b;
      }),
    );
    menu.hidden = false;
    const r = menu.getBoundingClientRect();
    menu.style.left = `${Math.min(x, window.innerWidth - r.width - 8)}px`;
    menu.style.top = `${Math.min(y, window.innerHeight - r.height - 8)}px`;
    (menu.firstElementChild as HTMLElement | null)?.focus();
  }
  function hideMenu() {
    menu.hidden = true;
  }
  document.addEventListener("pointerdown", (e) => {
    if (!menu.hidden && !menu.contains(e.target as Node)) hideMenu();
  });
  menu.onkeydown = (e) => {
    const items = [...menu.children] as HTMLElement[];
    const i = items.indexOf(document.activeElement as HTMLElement);
    if (e.key === "Escape") {
      hideMenu();
      list.focus();
    } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      items[(i + (e.key === "ArrowDown" ? 1 : items.length - 1)) % items.length]?.focus();
    }
  };
  window.addEventListener("blur", hideMenu);
  list.oncontextmenu = (e) => {
    if (e.target !== list || !client.ready) return;
    e.preventDefault();
    selected.clear();
    paintSelection();
    showMenu(e.clientX, e.clientY);
  };
  list.onclick = (e) => {
    if (e.target === list && selected.size) {
      selected.clear();
      paintSelection();
    }
  };

  // ---- keyboard ----

  list.onkeydown = (e) => {
    if (!client.ready || !cwd) return;
    const i = entries.findIndex((x) => selected.has(x.name));
    const mod = e.metaKey || e.ctrlKey;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (e.altKey && e.key === "ArrowUp") return void navigate(parent(cwd));
      const last = entries.length - 1;
      const next = i < 0 ? 0 : Math.min(last, Math.max(0, i + (e.key === "ArrowDown" ? 1 : -1)));
      if (entries.length) select(next, e.shiftKey ? (e as unknown as MouseEvent) : undefined);
    } else if (e.key === "Home" || e.key === "End") {
      e.preventDefault();
      if (entries.length) select(e.key === "Home" ? 0 : entries.length - 1);
    } else if (e.key === "Enter" && i >= 0) {
      e.preventDefault();
      void open(entries[i]);
    } else if (e.key === "Delete" || (e.key === "Backspace" && mod)) {
      e.preventDefault();
      void remove(selectedPaths());
    } else if (e.key === "Backspace") {
      e.preventDefault();
      void navigate(parent(cwd));
    } else if (e.key === "F2" && selected.size === 1) {
      e.preventDefault();
      void rename([...selected][0]);
    } else if (mod && e.key.toLowerCase() === "a") {
      e.preventDefault();
      selected = new Set(entries.map((x) => x.name));
      paintSelection();
    } else if (e.key === "Escape") {
      selected.clear();
      paintSelection();
    } else if (e.key === "ContextMenu" || (e.shiftKey && e.key === "F10")) {
      e.preventDefault();
      const row = list.children[Math.max(0, i)] as HTMLElement | undefined;
      const r = (row ?? list).getBoundingClientRect();
      showMenu(r.left + 24, r.bottom, i >= 0 ? entries[i] : undefined);
    }
  };

  // ---- drag out (Chrome/Edge: DownloadURL needs the data ready at dragstart) ----

  const cached = new Map<string, { key: string; blob: Blob; url: string }>();
  const fetching = new Set<string>();
  let prefetchTimer: ReturnType<typeof setTimeout> | undefined;

  function schedulePrefetch(e: AgentEntry) {
    clearTimeout(prefetchTimer);
    if (e.type === "file") prefetchTimer = setTimeout(() => prefetch(join(cwd, e.name), e), 300);
  }

  function prefetch(path: string, e: AgentEntry) {
    const key = `${e.size}:${e.mtime}`;
    if (e.size > DRAG_OUT_MAX || cached.get(path)?.key === key || fetching.has(path) || !client.ready) return;
    fetching.add(path);
    client
      .download(path, e.size)
      .then((blob) => {
        const old = cached.get(path);
        if (old) URL.revokeObjectURL(old.url);
        cached.delete(path);
        cached.set(path, { key, blob, url: URL.createObjectURL(blob) });
        while (cached.size > 12) {
          const [p, c] = cached.entries().next().value!;
          URL.revokeObjectURL(c.url);
          cached.delete(p);
        }
      })
      .catch(() => {})
      .finally(() => fetching.delete(path));
  }

  function dragStart(ev: DragEvent, e: AgentEntry, i: number) {
    if (!selected.has(e.name)) select(i);
    const paths = selectedPaths();
    const dt = ev.dataTransfer!;
    dt.effectAllowed = "copyMove";
    dt.setData(pathsType, JSON.stringify(paths));
    dt.setData("text/plain", paths.map(shellQuote).join(" "));
    const c = cached.get(join(cwd, e.name));
    if (paths.length === 1 && e.type === "file" && c?.key === `${e.size}:${e.mtime}`) {
      dt.setData("DownloadURL", `application/octet-stream:${e.name}:${c.url}`);
    }
  }

  // ---- drop targets ----

  const internal = (dt: DataTransfer | null) => !!dt?.types.includes(pathsType);
  const external = (dt: DataTransfer | null) => !!dt?.types.includes("Files") && !internal(dt);

  /** Makes el accept drops into dir (folder rows and crumbs). */
  function dropTarget(el: HTMLElement, dir: string) {
    el.addEventListener("dragover", (e) => {
      if (!client.ready || !(internal(e.dataTransfer) || external(e.dataTransfer))) return;
      e.preventDefault();
      e.stopPropagation();
      e.dataTransfer!.dropEffect = external(e.dataTransfer) || e.altKey ? "copy" : "move";
      el.classList.add("drop-into");
      showDrop(e.dataTransfer, dir, e.altKey);
    });
    el.addEventListener("dragleave", () => el.classList.remove("drop-into"));
    el.addEventListener("drop", (e) => {
      el.classList.remove("drop-into");
      if (!client.ready) return;
      e.preventDefault();
      e.stopPropagation();
      void onDrop(e, dir);
    });
  }

  function showDrop(dt: DataTransfer | null, dir: string, alt: boolean) {
    if (!external(dt) && dir === cwd) return hideDrop();
    const verb = external(dt) ? "Upload to" : alt ? "Copy to" : "Move to";
    dropText.textContent = `${verb} ${dir}`;
    drop.hidden = false;
  }
  const hideDrop = () => (drop.hidden = true);

  async function onDrop(e: DragEvent, dir: string) {
    hideDrop();
    const dt = e.dataTransfer!;
    if (internal(dt)) {
      const paths = JSON.parse(dt.getData(pathsType)) as string[];
      return moveInto(paths, dir, e.altKey);
    }
    upload(await readDrop(dt), dir);
  }

  // The whole panel takes uploads into the current folder.
  panel.addEventListener("dragover", (e) => {
    if (tab !== "files" || !client.ready || !cwd || !(internal(e.dataTransfer) || external(e.dataTransfer))) return;
    e.preventDefault();
    e.dataTransfer!.dropEffect = external(e.dataTransfer) ? "copy" : e.altKey ? "copy" : "none";
    showDrop(e.dataTransfer, cwd, e.altKey);
  });
  panel.addEventListener("dragleave", (e) => {
    if (!panel.contains(e.relatedTarget as Node)) hideDrop();
  });
  panel.addEventListener("drop", (e) => {
    if (tab !== "files" || !client.ready || !cwd) return;
    e.preventDefault();
    hideDrop();
    if (external(e.dataTransfer)) void onDrop(e, cwd);
    else if (internal(e.dataTransfer) && e.altKey) void onDrop(e, cwd); // duplicate here
  });
  document.addEventListener("dragend", hideDrop);

  // ---- machine state ----

  client.onReady = (ready) => {
    system.update();
    if (ready) {
      if (!panel.hidden && tab === "files") void navigate(cwd || client.home);
    } else {
      entries = [];
      for (const c of cached.values()) URL.revokeObjectURL(c.url);
      cached.clear();
    }
    render();
  };
  render();

  return {
    client,
    open: () => setOpen(true),
    /** Tracks the machine: the agent can only answer while it runs. */
    vmState(state: string, booted: boolean) {
      if (state === "running") {
        vmNote = booted ? "Waiting for the file agent…" : "Waiting for the guest to boot…";
        client.connect();
      } else {
        vmNote =
          state === "stopped"
            ? "The machine is stopped. Press Start to continue."
            : state === "halted"
              ? "The machine is powered off."
              : state === "error"
                ? "The machine stopped with an error."
                : "Waiting for the guest to boot…";
        client.reset(state === "stopped" ? "the machine was stopped" : "the guest restarted");
      }
      render();
      system.update();
    },
    /** Emulator stats from the worker, for the System tab. */
    emulator: (s: EmulatorStats) => system.emulator(s),
  };
}
