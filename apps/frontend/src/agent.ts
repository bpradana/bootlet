// Client for bootlet-agent, the guest program behind the file explorer. The
// worker passes bytes through unchanged; requests are matched to replies by
// id. The agent announces itself when it starts (id 0); until then, and
// again after a reboot, requests fail with "not ready".
import { agentChunk, encodeFrame, FrameDecoder, type AgentReply, type AgentRequest } from "@bootlet/shared";

export class AgentError extends Error {
  constructor(
    message: string,
    readonly code?: AgentReply["code"],
  ) {
    super(message);
  }
}

type Pending = { resolve: (r: { reply: AgentReply; data: Uint8Array }) => void; reject: (e: Error) => void };

export class AgentClient {
  ready = false;
  home = "/root";
  /** Called whenever ready changes. */
  onReady: (ready: boolean) => void = () => {};

  private dec = new FrameDecoder();
  private nextID = 1;
  private pending = new Map<number, Pending>();
  private helloTimer: ReturnType<typeof setInterval> | undefined;

  constructor(private send: (data: Uint8Array<ArrayBuffer>) => void) {}

  /**
   * Starts looking for the agent: data sent while it isn't running is lost,
   * so ask until it answers (it also announces itself when it starts).
   */
  connect() {
    if (this.helloTimer !== undefined || this.ready) return;
    const hello = () => {
      this.call({ op: "hello" }, undefined, true).then(
        (r) => this.setReady(r.reply),
        () => {},
      );
    };
    hello();
    this.helloTimer = setInterval(hello, 2000);
  }

  /** The guest stopped, rebooted or is booting: fail what is in flight. */
  reset(reason = "the guest restarted") {
    if (this.helloTimer !== undefined) clearInterval(this.helloTimer);
    this.helloTimer = undefined;
    for (const p of this.pending.values()) p.reject(new AgentError(reason));
    this.pending.clear();
    this.dec = new FrameDecoder();
    if (this.ready) {
      this.ready = false;
      this.onReady(false);
    }
  }

  /** Bytes from the agent. */
  feed(b: Uint8Array) {
    this.dec.feed(b);
    for (let f = this.dec.next(); f; f = this.dec.next()) {
      const { header, payload } = f;
      if (header.id === 0) {
        // The agent (re)started: anything in flight was lost.
        if (this.ready) this.reset("the file agent restarted");
        this.setReady(header);
        continue;
      }
      const p = this.pending.get(header.id);
      if (!p) continue;
      this.pending.delete(header.id);
      if (header.ok) p.resolve({ reply: header, data: payload });
      else p.reject(new AgentError(header.err ?? "failed", header.code));
    }
  }

  private setReady(r: AgentReply) {
    if (this.helloTimer !== undefined) clearInterval(this.helloTimer);
    this.helloTimer = undefined;
    if (r.home) this.home = r.home;
    if (!this.ready) {
      this.ready = true;
      this.onReady(true);
    }
  }

  call(req: AgentRequest, payload?: Uint8Array, beforeReady = false): Promise<{ reply: AgentReply; data: Uint8Array }> {
    if (!this.ready && !beforeReady) return Promise.reject(new AgentError("the guest is not ready"));
    const id = this.nextID++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.send(encodeFrame({ ...req, id }, payload));
    });
  }

  async list(path: string) {
    return (await this.call({ op: "list", path })).reply.entries ?? [];
  }

  async stat(path: string) {
    return (await this.call({ op: "stat", path })).reply.entry!;
  }

  /** Whether path exists (false only for "not found"; other errors throw). */
  async exists(path: string) {
    try {
      await this.stat(path);
      return true;
    } catch (e) {
      if (e instanceof AgentError && e.code === "notexist") return false;
      throw e;
    }
  }

  async op(op: "mkdir" | "remove", path: string) {
    await this.call({ op, path });
  }

  async move(op: "rename" | "copy", path: string, to: string, overwrite = false) {
    await this.call({ op, path, to, overwrite });
  }

  /** Reads a whole file into a Blob. */
  async download(path: string, size: number, progress?: (done: number) => void, signal?: AbortSignal): Promise<Blob> {
    const parts: Uint8Array<ArrayBuffer>[] = [];
    let off = 0;
    for (;;) {
      signal?.throwIfAborted();
      const { reply, data } = await this.call({ op: "read", path, off, len: agentChunk });
      parts.push(data as Uint8Array<ArrayBuffer>);
      off += data.length;
      progress?.(Math.min(off, size));
      if (reply.eof || data.length === 0) return new Blob(parts);
    }
  }

  /**
   * Writes a file. It goes to a temporary name first and is renamed into
   * place at the end, so an interrupted upload never looks complete.
   */
  async upload(path: string, file: Blob, progress?: (done: number) => void, signal?: AbortSignal) {
    const part = `${path}.bootlet-part`;
    try {
      let off = 0;
      do {
        signal?.throwIfAborted();
        const chunk = new Uint8Array(await file.slice(off, off + agentChunk).arrayBuffer());
        await this.call({ op: "write", path: part, off, trunc: off === 0 }, chunk);
        off += chunk.length;
        progress?.(off);
      } while (off < file.size);
      await this.move("rename", part, path, true);
    } catch (e) {
      if (this.ready) void this.op("remove", part).catch(() => {});
      throw e;
    }
  }
}
