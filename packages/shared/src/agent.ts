// Protocol of bootlet-agent, the guest program behind the file explorer (Go:
// packages/shared/agent). Frames are the magic "BLA1", the header and payload
// lengths (u32 little endian), a JSON header and the payload bytes.

export const agentMagic = "BLA1";
/** Read and write size, as in the Go client. */
export const agentChunk = 256 << 10;
const maxHeader = 8 << 20;
const maxPayload = 1 << 20;

export type AgentOp = "hello" | "list" | "stat" | "read" | "write" | "mkdir" | "rename" | "copy" | "remove";

export interface AgentRequest {
  id?: number;
  op: AgentOp;
  path?: string;
  to?: string;
  off?: number;
  len?: number;
  trunc?: boolean;
  overwrite?: boolean;
}

export interface AgentEntry {
  name: string;
  type: "file" | "dir" | "link" | "other";
  size: number;
  mode: number;
  /** Unix milliseconds. */
  mtime: number;
  target?: string;
  targetDir?: boolean;
}

export interface AgentReply {
  id: number;
  ok: boolean;
  err?: string;
  code?: "exist" | "notexist" | "perm";
  version?: number;
  home?: string;
  entries?: AgentEntry[];
  entry?: AgentEntry;
  eof?: boolean;
}

const magic = new TextEncoder().encode(agentMagic);

export function encodeFrame(header: object, payload: Uint8Array = new Uint8Array()): Uint8Array<ArrayBuffer> {
  const h = new TextEncoder().encode(JSON.stringify(header));
  const f = new Uint8Array(12 + h.length + payload.length);
  f.set(magic);
  const v = new DataView(f.buffer);
  v.setUint32(4, h.length, true);
  v.setUint32(8, payload.length, true);
  f.set(h, 12);
  f.set(payload, 12 + h.length);
  return f;
}

/** Splits the agent's byte stream into frames, skipping bytes that don't start one. */
export class FrameDecoder {
  private buf = new Uint8Array(0);

  feed(b: Uint8Array) {
    const n = new Uint8Array(this.buf.length + b.length);
    n.set(this.buf);
    n.set(b, this.buf.length);
    this.buf = n;
  }

  next(): { header: AgentReply; payload: Uint8Array } | undefined {
    for (;;) {
      const i = indexOf(this.buf, magic);
      if (i < 0) {
        this.buf = this.buf.slice(Math.max(0, this.buf.length - magic.length + 1));
        return undefined;
      }
      this.buf = this.buf.subarray(i);
      if (this.buf.length < 12) return undefined;
      const v = new DataView(this.buf.buffer, this.buf.byteOffset);
      const hl = v.getUint32(4, true);
      const pl = v.getUint32(8, true);
      if (hl === 0 || hl > maxHeader || pl > maxPayload) {
        this.buf = this.buf.subarray(1);
        continue;
      }
      if (this.buf.length < 12 + hl + pl) return undefined;
      const header = JSON.parse(new TextDecoder().decode(this.buf.subarray(12, 12 + hl))) as AgentReply;
      const payload = this.buf.slice(12 + hl, 12 + hl + pl);
      this.buf = this.buf.slice(12 + hl + pl);
      return { header, payload };
    }
  }
}

function indexOf(hay: Uint8Array, needle: Uint8Array): number {
  outer: for (let i = 0; i + needle.length <= hay.length; i++) {
    for (let j = 0; j < needle.length; j++) if (hay[i + j] !== needle[j]) continue outer;
    return i;
  }
  return -1;
}
