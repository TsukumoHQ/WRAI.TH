import { Camera } from "./camera.js";

export class CanvasEngine {
  constructor(canvasEl) {
    this.canvas = canvasEl;
    this.ctx = canvasEl.getContext("2d");
    this.renderables = [];
    this.running = false;
    this.lastTime = 0;
    this.camera = new Camera();
    this.onResize = null; // optional callback fired after the bitmap is re-rasterized

    this.resize();
    window.addEventListener("resize", () => this.resize());
    // Re-rasterize whenever the container box changes (panel overlays, mode
    // switches, dock drags) — not just on window resize. Without this the
    // bitmap gets CSS-stretched and sprites deform.
    if (typeof ResizeObserver !== "undefined") {
      this._ro = new ResizeObserver(() => this.resize());
      this._ro.observe(this.canvas.parentElement);
    }
  }

  resize() {
    const container = this.canvas.parentElement;
    const w = container.clientWidth;
    const h = container.clientHeight;
    // Container hidden (display:none mode) — keep the last good bitmap.
    if (!w || !h) return;
    const dpr = window.devicePixelRatio || 1;
    // No-op if nothing actually changed (synthetic resize events).
    if (w === this.width && h === this.height && this.canvas.width === Math.round(w * dpr)) return;
    this.canvas.width = Math.round(w * dpr);
    this.canvas.height = Math.round(h * dpr);
    this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    this.width = w;
    this.height = h;
    if (this.onResize) this.onResize();
  }

  add(renderable) {
    this.renderables.push(renderable);
  }

  remove(renderable) {
    const idx = this.renderables.indexOf(renderable);
    if (idx !== -1) this.renderables.splice(idx, 1);
  }

  start() {
    if (this.running) return;
    this.running = true;
    this.lastTime = performance.now();
    this._frame();
  }

  _frame() {
    if (!this.running) return;
    const now = performance.now();
    const dt = (now - this.lastTime) / 1000;
    this.lastTime = now;

    const { ctx, width: w, height: h } = this;
    ctx.clearRect(0, 0, w, h);

    // Update camera
    this.camera.update(dt);

    // Sort by y for depth (use world-space y)
    this.renderables.sort((a, b) => (a.y ?? 0) - (b.y ?? 0));

    for (const r of this.renderables) {
      if (r.update) r.update(dt);

      if (r.isBackground) {
        // Render backgrounds without camera transform (fills viewport)
        if (r.render) r.render(ctx, w, h);
      } else {
        // Render world-space objects with camera transform
        ctx.save();
        this.camera.apply(ctx, w, h);
        if (r.render) r.render(ctx, w, h);
        ctx.restore();
      }
    }

    requestAnimationFrame(() => this._frame());
  }

  stop() {
    this.running = false;
  }
}
