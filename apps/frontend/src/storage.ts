// Persistent storage for the guest's /data disk. Only blocks the guest has
// written are stored. OPFS is preferred (fast, synchronous access in
// workers); IndexedDB is the fallback where OPFS sync handles are missing.

const BLOCK = 4096;

export interface Blocks {
  idx: Uint32Array;
  data: Uint8Array; // idx.length * 4096 bytes
}

export interface BlockStore {
  kind: "opfs" | "indexeddb";
  load(): Promise<Blocks>;
  save(blocks: Blocks): Promise<void>;
  clear(): Promise<void>;
}

export async function openStore(): Promise<BlockStore> {
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
// 64 MiB /data disk) must not be mistaken for the current root filesystem.
const NAME = "bootlet-root-v1";

async function openOPFS(): Promise<BlockStore> {
  const dir = await (await navigator.storage.getDirectory()).getDirectoryHandle(NAME, { create: true });
  const open = async (name: string) =>
    (await dir.getFileHandle(name, { create: true })).createSyncAccessHandle();
  // Compact layout: "blocks" holds stored blocks back to back in slot order,
  // and "index" holds the disk block number of each slot. Writing blocks at
  // their disk offsets instead would make the file as long as the disk
  // (1 GiB), and browsers charge OPFS quota by file length.
  const blocks = await open("blocks");
  let index: FileSystemSyncAccessHandle;
  try {
    index = await open("index");
  } catch (e) {
    blocks.close(); // release the lock so a retry can succeed
    throw e;
  }

  const slots = new Map<number, number>(); // disk block → slot
  return {
    kind: "opfs",
    async load() {
      const idx = new Uint32Array(index.getSize() / 4);
      index.read(idx, { at: 0 });
      const data = new Uint8Array(idx.length * BLOCK);
      blocks.read(data, { at: 0 });
      idx.forEach((b, slot) => slots.set(b, slot));
      return { idx, data };
    },
    async save({ idx, data }) {
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
    },
    async clear() {
      slots.clear();
      blocks.truncate(0);
      index.truncate(0);
      blocks.flush();
      index.flush();
    },
  };
}

function request<T>(r: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
}

async function openIndexedDB(): Promise<BlockStore> {
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
