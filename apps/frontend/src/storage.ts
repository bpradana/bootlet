// Persistent storage for the guest's root disk. Only blocks the guest has
// written are stored. OPFS is preferred: its synchronous access handles let
// the emulator read blocks on demand, so the disk's contents don't have to
// sit in memory. IndexedDB is the fallback where OPFS sync handles are
// missing; it is asynchronous, so everything is loaded into memory at boot.

const BLOCK = 4096;

export interface Blocks {
  idx: Uint32Array;
  data: Uint8Array; // idx.length * 4096 bytes
}

interface StoreBase {
  save(blocks: Blocks): Promise<void>;
  clear(): Promise<void>;
}

export interface OPFSStore extends StoreBase {
  kind: "opfs";
  /** Whether nothing has been stored yet (a fresh disk). */
  empty(): boolean;
  /** Synchronously fills out with blocks idx; never-stored blocks read as zeros. */
  readBlocks(idx: Uint32Array, out: Uint8Array): void;
}

export interface IndexedDBStore extends StoreBase {
  kind: "indexeddb";
  load(): Promise<Blocks>;
}

export type BlockStore = OPFSStore | IndexedDBStore;

// Disks from earlier layouts, deleted when found: they were formatted
// without a journal and can't be upgraded in place.
const OLD_NAMES = ["bootlet", "bootlet-root-v1"];

async function removeOldDisks() {
  const root = await navigator.storage.getDirectory();
  for (const name of OLD_NAMES) {
    await root.removeEntry(name, { recursive: true }).catch(() => {});
    indexedDB.deleteDatabase(name);
  }
}

export async function openStore(): Promise<BlockStore> {
  await removeOldDisks().catch(() => {});
  // After a reload the previous page's worker may still hold the OPFS lock
  // for a moment, so retry briefly before concluding another tab has it.
  for (let attempt = 0; ; attempt++) {
    try {
      return await openOPFS();
    } catch (e) {
      if (!(e instanceof DOMException && e.name === "NoModificationAllowedError")) {
        return await openIndexedDB();
      }
      // Falling back to IndexedDB here would silently fork the disk.
      if (attempt >= 10) throw new Error("the persistent disk is open in another tab");
      await new Promise((r) => setTimeout(r, 200));
    }
  }
}

// Storage names carry a version: a disk from an incompatible layout (the old
// unjournaled disks) must not be mistaken for the current root filesystem.
const NAME = "bootlet-root-v2";

const WAL_MAGIC = 0x4c415742; // "BWAL"

async function openOPFS(): Promise<OPFSStore> {
  const dir = await (await navigator.storage.getDirectory()).getDirectoryHandle(NAME, { create: true });
  // Compact layout: "blocks" holds stored blocks back to back in slot order,
  // and "index" holds the disk block number of each slot. Writing blocks at
  // their disk offsets instead would make the file as long as the disk
  // (1 GiB), and browsers charge OPFS quota by file length.
  //
  // Each save goes through "wal" first, so a page closed mid-save leaves
  // either the whole batch or none of it. The guest's journal relies on
  // that: the stored disk must always be the device as of some moment.
  const handles: FileSystemSyncAccessHandle[] = [];
  try {
    for (const name of ["blocks", "index", "wal"]) {
      handles.push(await (await dir.getFileHandle(name, { create: true })).createSyncAccessHandle());
    }
  } catch (e) {
    handles.forEach((h) => h.close()); // release the locks so a retry can succeed
    throw e;
  }
  const [blocks, index, wal] = handles;

  const slots = new Map<number, number>(); // disk block → slot
  const apply = ({ idx, data }: Blocks) => {
    idx.forEach((b, i) => {
      let slot = slots.get(b);
      if (slot === undefined) {
        slot = slots.size;
        slots.set(b, slot);
        index.write(Uint32Array.of(b), { at: slot * 4 });
      }
      blocks.write(data.subarray(i * BLOCK, (i + 1) * BLOCK), { at: slot * BLOCK });
    });
    blocks.flush();
    index.flush();
    wal.truncate(0);
    wal.flush();
  };

  // A complete batch left in the log was interrupted after it was logged:
  // apply it again. An incomplete one never happened.
  const recover = () => {
    const size = wal.getSize();
    if (size < 12) return;
    const head = new Uint32Array(2);
    wal.read(head, { at: 0 });
    const n = head[1];
    const end = 8 + n * 4 + n * BLOCK;
    const trailer = new Uint32Array(1);
    if (head[0] !== WAL_MAGIC || size !== end + 4 || (wal.read(trailer, { at: end }), trailer[0] !== WAL_MAGIC)) {
      wal.truncate(0);
      return;
    }
    const idx = new Uint32Array(n);
    const data = new Uint8Array(n * BLOCK);
    wal.read(idx, { at: 8 });
    wal.read(data, { at: 8 + n * 4 });
    apply({ idx, data });
  };

  const stored = new Uint32Array(index.getSize() / 4);
  index.read(stored, { at: 0 });
  stored.forEach((b, slot) => slots.set(b, slot));
  recover();

  return {
    kind: "opfs",
    empty: () => slots.size === 0,
    readBlocks(idx, out) {
      idx.forEach((b, i) => {
        const dst = out.subarray(i * BLOCK, (i + 1) * BLOCK);
        const slot = slots.get(b);
        if (slot === undefined) dst.fill(0);
        else blocks.read(dst, { at: slot * BLOCK });
      });
    },
    async save(batch) {
      const n = batch.idx.length;
      wal.truncate(0);
      wal.write(Uint32Array.of(WAL_MAGIC, n), { at: 0 });
      wal.write(batch.idx, { at: 8 });
      wal.write(batch.data, { at: 8 + n * 4 });
      wal.write(Uint32Array.of(WAL_MAGIC), { at: 8 + n * 4 + n * BLOCK }); // written last: the batch is complete
      wal.flush();
      apply(batch);
    },
    async clear() {
      slots.clear();
      for (const h of [blocks, index, wal]) {
        h.truncate(0);
        h.flush();
      }
    },
  };
}

function request<T>(r: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
}

async function openIndexedDB(): Promise<IndexedDBStore> {
  const open = indexedDB.open(NAME, 1);
  open.onupgradeneeded = () => open.result.createObjectStore("blocks");
  const db = await request(open);
  return {
    kind: "indexeddb",
    async load() {
      const tx = db.transaction("blocks", "readonly").objectStore("blocks");
      const [keys, values] = await Promise.all([
        request(tx.getAllKeys()) as Promise<number[]>,
        request(tx.getAll()) as Promise<Uint8Array[]>,
      ]);
      const data = new Uint8Array(keys.length * BLOCK);
      values.forEach((v, i) => data.set(v, i * BLOCK));
      return { idx: Uint32Array.from(keys), data };
    },
    async save({ idx, data }) {
      const tx = db.transaction("blocks", "readwrite");
      const store = tx.objectStore("blocks");
      idx.forEach((b, i) => store.put(data.slice(i * BLOCK, (i + 1) * BLOCK), b));
      await new Promise<void>((resolve, reject) => {
        tx.oncomplete = () => resolve();
        tx.onerror = () => reject(tx.error);
      });
    },
    async clear() {
      await request(db.transaction("blocks", "readwrite").objectStore("blocks").clear());
    },
  };
}
