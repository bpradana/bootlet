// The guest's graphical display: a canvas above the serial console that the
// worker draws into, plus the virtio keyboard. While the canvas has focus,
// key presses and releases go to the guest as Linux input events (good for
// games: no auto-repeat, real key-up). Every held key is released when the
// canvas loses focus, the window blurs, the tab is hidden or the display is
// closed, so the guest never sees a key stuck down.
import { display, type ToWorker } from "@bootlet/shared";

const SHOWN_KEY = "bootlet.display";
const HEIGHT_KEY = "bootlet.displayHeight";

const store = {
  get: (k: string) => {
    try {
      return localStorage.getItem(k);
    } catch {
      return null;
    }
  },
  set: (k: string, v: string) => {
    try {
      localStorage.setItem(k, v);
    } catch {
      // not remembered
    }
  },
};

/** KeyboardEvent.code → Linux KEY_* code (include/uapi/linux/input-event-codes.h). */
export const keyCodes: Record<string, number> = {
  Escape: 1, Digit1: 2, Digit2: 3, Digit3: 4, Digit4: 5, Digit5: 6, Digit6: 7, Digit7: 8, Digit8: 9, Digit9: 10,
  Digit0: 11, Minus: 12, Equal: 13, Backspace: 14, Tab: 15, KeyQ: 16, KeyW: 17, KeyE: 18, KeyR: 19, KeyT: 20,
  KeyY: 21, KeyU: 22, KeyI: 23, KeyO: 24, KeyP: 25, BracketLeft: 26, BracketRight: 27, Enter: 28, ControlLeft: 29,
  KeyA: 30, KeyS: 31, KeyD: 32, KeyF: 33, KeyG: 34, KeyH: 35, KeyJ: 36, KeyK: 37, KeyL: 38, Semicolon: 39,
  Quote: 40, Backquote: 41, ShiftLeft: 42, Backslash: 43, KeyZ: 44, KeyX: 45, KeyC: 46, KeyV: 47, KeyB: 48,
  KeyN: 49, KeyM: 50, Comma: 51, Period: 52, Slash: 53, ShiftRight: 54, NumpadMultiply: 55, AltLeft: 56,
  Space: 57, CapsLock: 58, F1: 59, F2: 60, F3: 61, F4: 62, F5: 63, F6: 64, F7: 65, F8: 66, F9: 67, F10: 68,
  NumLock: 69, ScrollLock: 70, Numpad7: 71, Numpad8: 72, Numpad9: 73, NumpadSubtract: 74, Numpad4: 75,
  Numpad5: 76, Numpad6: 77, NumpadAdd: 78, Numpad1: 79, Numpad2: 80, Numpad3: 81, Numpad0: 82,
  NumpadDecimal: 83, IntlBackslash: 86, F11: 87, F12: 88, NumpadEnter: 96, ControlRight: 97, NumpadDivide: 98,
  AltRight: 100, Home: 102, ArrowUp: 103, PageUp: 104, ArrowLeft: 105, ArrowRight: 106, End: 107,
  ArrowDown: 108, PageDown: 109, Insert: 110, Delete: 111, Pause: 119,
};

export interface Display {
  /** Draws a frame sent by the worker (when the canvas couldn't be transferred). */
  frame(rects: Int32Array, data: Uint8Array): void;
  /** Releases every held key (also done automatically on focus loss). */
  releaseAll(): void;
  shown(): boolean;
  /** Replaces the canvas with one the worker draws with 2D (the GPU failed). */
  fallback(): void;
}

export function initDisplay(opts: {
  send: (msg: ToWorker, transfer?: Transferable[]) => void;
  onHide: () => void;
  renderer: string; // "webgpu", "2d" or "" (WebGPU when available)
  offscreen: boolean; // hand the canvas to the worker
}): Display {
  const $ = <T extends HTMLElement = HTMLElement>(id: string) => document.getElementById(id) as T;
  const pane = $("display-pane");
  let canvas = $<HTMLCanvasElement>("display");
  const splitter = $("display-splitter");
  const toggleBtn = $<HTMLButtonElement>("display-toggle");
  const hint = $("display-hint");
  canvas.width = display.width;
  canvas.height = display.height;

  // By default the worker posts changed regions and the page draws them
  // with 2D. With opts.offscreen the canvas goes to the worker instead
  // (pixels never cross threads, and WebGPU draws at the canvas's real size
  // in device pixels), but Chrome then shows only a fraction of the frames
  // while the guest is busy.
  let ctx: CanvasRenderingContext2D | null = null;
  const ro = new ResizeObserver(([e]) => {
    const box = e.devicePixelContentBoxSize?.[0];
    const width = box ? box.inlineSize : Math.round(e.contentRect.width * devicePixelRatio);
    const height = box ? box.blockSize : Math.round(e.contentRect.height * devicePixelRatio);
    if (width > 0 && height > 0) opts.send({ type: "canvasSize", width, height });
  });
  function transfer(renderer: string) {
    const off = canvas.transferControlToOffscreen();
    opts.send({ type: "canvas", canvas: off, renderer }, [off]);
    try {
      ro.observe(canvas, { box: "device-pixel-content-box" });
    } catch {
      ro.observe(canvas); // Safari: content-box, scaled by devicePixelRatio
    }
  }
  if (opts.offscreen && "transferControlToOffscreen" in canvas) {
    transfer(opts.renderer);
  } else {
    ctx = (canvas as HTMLCanvasElement).getContext("2d", { alpha: false });
    opts.send({ type: "frames" });
  }

  // ---- keyboard ----

  const held = new Set<number>();
  const key = (code: number, down: boolean) => opts.send({ type: "key", code, down });

  function releaseAll() {
    for (const code of held) key(code, false);
    held.clear();
  }

  function listen(c: HTMLCanvasElement) {
    c.addEventListener("keydown", (e) => {
      const code = keyCodes[e.code];
      // Leave browser shortcuts (Cmd/Meta combinations) to the browser.
      if (code === undefined || e.metaKey) return;
      e.preventDefault();
      e.stopPropagation();
      if (e.repeat || held.has(code)) return; // the guest sees one press per key
      held.add(code);
      key(code, true);
    });
    c.addEventListener("keyup", (e) => {
      const code = keyCodes[e.code];
      if (code === undefined) return;
      e.preventDefault();
      if (!held.delete(code)) return;
      key(code, false);
    });
    c.addEventListener("blur", releaseAll);
    c.addEventListener("pointerdown", () => c.focus());
    c.addEventListener("focus", () => (hint.dataset.focus = ""));
    c.addEventListener("blur", () => delete hint.dataset.focus);
  }
  listen(canvas);
  window.addEventListener("blur", releaseAll);
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) releaseAll();
  });

  // ---- layout ----

  function setHeight(px: number) {
    const max = pane.parentElement!.getBoundingClientRect().height - 80;
    pane.style.height = `${Math.round(Math.max(120, Math.min(px, max)))}px`;
  }
  const saved = Number(store.get(HEIGHT_KEY));
  if (saved > 0) setHeight(saved);

  function setShown(on: boolean) {
    pane.hidden = !on;
    splitter.hidden = !on;
    toggleBtn.setAttribute("aria-pressed", String(on));
    store.set(SHOWN_KEY, on ? "1" : "0");
    if (!on) {
      releaseAll();
      opts.onHide();
    } else {
      opts.send({ type: "redraw" });
    }
  }
  toggleBtn.onclick = () => {
    setShown(!!pane.hidden);
    if (!pane.hidden) canvas.focus();
  };
  setShown(store.get(SHOWN_KEY) !== "0");

  splitter.onpointerdown = (e) => {
    splitter.setPointerCapture(e.pointerId);
    const top = pane.getBoundingClientRect().top;
    splitter.onpointermove = (ev) => setHeight(ev.clientY - top);
    splitter.onpointerup = () => {
      splitter.onpointermove = splitter.onpointerup = null;
      store.set(HEIGHT_KEY, String(pane.getBoundingClientRect().height));
    };
  };
  splitter.onkeydown = (e) => {
    const step = e.key === "ArrowUp" ? -24 : e.key === "ArrowDown" ? 24 : 0;
    if (!step) return;
    e.preventDefault();
    setHeight(pane.getBoundingClientRect().height + step);
    store.set(HEIGHT_KEY, String(pane.getBoundingClientRect().height));
  };

  return {
    frame(rects, data) {
      if (!ctx) return;
      for (let i = 0; i < rects.length; i += 5) {
        const [x, y, w, h, off] = rects.subarray(i, i + 5);
        ctx.putImageData(new ImageData(new Uint8ClampedArray(data.buffer as ArrayBuffer, data.byteOffset + off, w * h * 4), w, h), x, y);
      }
    },
    releaseAll,
    shown: () => !pane.hidden,
    fallback() {
      // The worker's GPU device is gone: a canvas keeps its first context
      // type, so draw into a fresh one with 2D.
      const focused = document.activeElement === canvas;
      ro.unobserve(canvas);
      const fresh = canvas.cloneNode(false) as HTMLCanvasElement;
      canvas.replaceWith(fresh);
      canvas = fresh;
      listen(canvas);
      transfer("2d");
      if (focused) canvas.focus();
    },
  };
}
