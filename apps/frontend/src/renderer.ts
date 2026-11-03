// Draws the guest display into the page's canvas from the worker. Both
// renderers take the same input: changed regions as [x, y, w, h, offset]
// quintuples with RGBA pixels packed row by row from offset in data.
//
// - "webgpu" keeps the screen in a texture, uploads only changed regions,
//   and draws it at the canvas's real device-pixel size with sharp bilinear
//   filtering: every guest pixel stays square and crisp at any scale, without
//   the uneven pixel widths of nearest-neighbour scaling.
// - "2d" puts the regions into a canvas of the guest's size and lets CSS
//   scale it (the fallback, and where WebGPU is unavailable).

export type RendererKind = "webgpu" | "2d";

export interface Renderer {
  kind: RendererKind;
  /** Applies changed regions. */
  draw(rects: Int32Array, data: Uint8Array): void;
  /** The canvas's displayed size in device pixels changed. */
  resize(width: number, height: number): void;
}

/** Picks WebGPU when available (unless want is "2d"), else 2D. */
export async function createRenderer(
  canvas: OffscreenCanvas,
  width: number,
  height: number,
  want: string,
  onLost: () => void,
): Promise<Renderer | undefined> {
  if (want !== "2d") {
    try {
      const r = await createWebGPU(canvas, width, height, onLost);
      if (r) return r;
    } catch (e) {
      console.warn("WebGPU display unavailable, using 2D:", e);
    }
  }
  return create2D(canvas, width, height);
}

function create2D(canvas: OffscreenCanvas, width: number, height: number): Renderer | undefined {
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext("2d", { alpha: false });
  if (!ctx) return undefined;
  return {
    kind: "2d",
    draw(rects, data) {
      for (let i = 0; i < rects.length; i += 5) {
        const [x, y, w, h, off] = rects.subarray(i, i + 5);
        const px = new Uint8ClampedArray(data.buffer as ArrayBuffer, data.byteOffset + off, w * h * 4);
        ctx.putImageData(new ImageData(px, w, h), x, y);
      }
    },
    resize() {}, // CSS scales the guest-sized canvas
  };
}

// Objects kept alive for the life of the worker.
const keep: unknown[] = [];

// GPUTextureUsage flags (spelled out: the constants aren't in TypeScript's lib).
const COPY_DST = 0x02;
const TEXTURE_BINDING = 0x04;

const shader = /* wgsl */ `
@group(0) @binding(0) var screen: texture_2d<f32>;
@group(0) @binding(1) var smp: sampler;
// scale: canvas pixels per guest pixel; the guest image is letterboxed in
// the canvas (offset and size in canvas pixels).
struct View { offset: vec2f, size: vec2f, scale: f32 }
@group(0) @binding(2) var<uniform> view: View;

@vertex fn vs(@builtin(vertex_index) i: u32) -> @builtin(position) vec4f {
  let p = vec2f(f32((i << 1u) & 2u), f32(i & 2u)); // one triangle covering the canvas
  return vec4f(p * 2.0 - 1.0, 0.0, 1.0);
}

@fragment fn fs(@builtin(position) pos: vec4f) -> @location(0) vec4f {
  let uv = (pos.xy - view.offset) / view.size;
  if (any(uv < vec2f(0.0)) || any(uv >= vec2f(1.0))) {
    return vec4f(0.0, 0.0, 0.0, 1.0);
  }
  // Sharp bilinear: nearest within each guest pixel, a one-canvas-pixel
  // blend at its edges.
  let dims = vec2f(textureDimensions(screen));
  let texel = uv * dims;
  let f = fract(texel) - 0.5;
  let edge = 0.5 - 0.5 / max(view.scale, 1.0);
  let t = floor(texel) + (f - clamp(f, vec2f(-edge), vec2f(edge))) * max(view.scale, 1.0) + 0.5;
  return vec4f(textureSampleLevel(screen, smp, t / dims, 0.0).rgb, 1.0);
}
`;

async function createWebGPU(
  canvas: OffscreenCanvas,
  width: number,
  height: number,
  onLost: () => void,
): Promise<Renderer | undefined> {
  if (!("gpu" in navigator) || !navigator.gpu) return undefined;
  const adapter = await navigator.gpu.requestAdapter();
  if (!adapter) return undefined;
  const device = await adapter.requestDevice();
  keep.push(adapter, device); // Chromium loses the device if these are collected
  const ctx = canvas.getContext("webgpu") as GPUCanvasContext | null;
  if (!ctx) return undefined;
  const format = navigator.gpu.getPreferredCanvasFormat();
  ctx.configure({ device, format, alphaMode: "opaque" });

  const texture = device.createTexture({ size: [width, height], format: "rgba8unorm", usage: COPY_DST | TEXTURE_BINDING });
  const sampler = device.createSampler({ magFilter: "linear", minFilter: "linear" });
  const uniforms = device.createBuffer({ size: 32, usage: 0x40 /* UNIFORM */ | 0x08 /* COPY_DST */ });
  const module = device.createShaderModule({ code: shader });
  const pipeline = device.createRenderPipeline({
    layout: "auto",
    vertex: { module, entryPoint: "vs" },
    fragment: { module, entryPoint: "fs", targets: [{ format }] },
    primitive: { topology: "triangle-list" },
  });
  const bind = device.createBindGroup({
    layout: pipeline.getBindGroupLayout(0),
    entries: [
      { binding: 0, resource: texture.createView() },
      { binding: 1, resource: sampler },
      { binding: 2, resource: { buffer: uniforms } },
    ],
  });

  let lost = false;
  void device.lost.then((info) => {
    lost = true;
    console.warn("WebGPU device lost:", info.message);
    onLost();
  });

  // Fit the guest's aspect ratio into the canvas.
  function layout() {
    const scale = Math.min(canvas.width / width, canvas.height / height);
    const w = width * scale;
    const h = height * scale;
    const v = new Float32Array([(canvas.width - w) / 2, (canvas.height - h) / 2, w, h, scale, 0, 0, 0]);
    device.queue.writeBuffer(uniforms, 0, v);
  }

  function render() {
    if (lost || canvas.width === 0 || canvas.height === 0) return;
    const enc = device.createCommandEncoder();
    const pass = enc.beginRenderPass({
      colorAttachments: [{ view: ctx!.getCurrentTexture().createView(), loadOp: "clear", storeOp: "store", clearValue: [0, 0, 0, 1] }],
    });
    pass.setPipeline(pipeline);
    pass.setBindGroup(0, bind);
    pass.draw(3);
    pass.end();
    device.queue.submit([enc.finish()]);
  }

  canvas.width = width;
  canvas.height = height;
  layout();
  return {
    kind: "webgpu",
    draw(rects, data) {
      if (lost) return;
      for (let i = 0; i < rects.length; i += 5) {
        const [x, y, w, h, off] = rects.subarray(i, i + 5);
        device.queue.writeTexture(
          { texture, origin: [x, y] },
          data as Uint8Array<ArrayBuffer>,
          { offset: off, bytesPerRow: w * 4, rowsPerImage: h },
          [w, h],
        );
      }
      render();
    },
    resize(w, h) {
      if (lost || w <= 0 || h <= 0 || (w === canvas.width && h === canvas.height)) return;
      canvas.width = w;
      canvas.height = h;
      layout();
      render();
    },
  };
}
