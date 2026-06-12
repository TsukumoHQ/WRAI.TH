// MCP Event Visual Effects
// Triggered by SSE events from /api/events/stream
// Each MCP tool group has distinct pixel-art-style canvas animations.

// ─── EFFECT COLORS ──────────────────────────────────────────────────
const COLORS = {
  memory:   { core: "#00bcd4", glow: "#0097a7", accent: "#b2ebf2" },  // cyan
  conflict: { core: "#ff9800", glow: "#e65100", accent: "#ffe0b2" },  // amber
  resolve:  { core: "#4caf50", glow: "#2e7d32", accent: "#c8e6c9" },  // green
  task:     { core: "#ff006e", glow: "#c51162", accent: "#ff80ab" },  // hot pink
  complete: { core: "#00e676", glow: "#00c853", accent: "#b9f6ca" },  // green
  block:    { core: "#ff1744", glow: "#d50000", accent: "#ff8a80" },  // red
  register: { core: "#7c4dff", glow: "#6200ea", accent: "#b388ff" },  // purple
  sleep:    { core: "#9575cd", glow: "#512da8", accent: "#d1c4e9" },  // lavender
};

// ─── FLOATING TEXT ──────────────────────────────────────────────────
class FloatingText {
  constructor(x, y, text, color, duration = 1.5) {
    this.x = x;
    this.y = y;
    this.text = text;
    this.color = color;
    this.life = duration;
    this.maxLife = duration;
    this.vy = -30;
  }

  get alive() { return this.life > 0; }

  update(dt) {
    this.life -= dt;
    this.y += this.vy * dt;
    this.vy *= 0.97; // decelerate
  }

  render(ctx) {
    const alpha = Math.max(0, this.life / this.maxLife);
    const scale = 0.8 + 0.2 * (1 - alpha); // slight grow as it fades
    ctx.save();
    ctx.globalAlpha = alpha;
    ctx.font = `bold ${Math.round(11 * scale)}px "JetBrains Mono", monospace`;
    ctx.textAlign = "center";
    ctx.textBaseline = "middle";
    // Shadow
    ctx.fillStyle = "#000";
    ctx.fillText(this.text, this.x + 1, this.y + 1);
    // Text
    ctx.fillStyle = this.color;
    ctx.fillText(this.text, this.x, this.y);
    ctx.restore();
  }
}

// ─── GLYPH RISE (icon that rises and fades) ─────────────────────────
class GlyphRise {
  constructor(x, y, glyph, color, glowColor, duration = 2.0) {
    this.x = x;
    this.y = y;
    this.glyph = glyph;
    this.color = color;
    this.glowColor = glowColor;
    this.life = duration;
    this.maxLife = duration;
    this.vy = -20;
    this.phase = Math.random() * Math.PI * 2;
  }

  get alive() { return this.life > 0; }

  update(dt) {
    this.life -= dt;
    this.y += this.vy * dt;
    this.vy *= 0.98;
    this.phase += dt * 4;
    this.x += Math.sin(this.phase) * 0.3; // gentle sway
  }

  render(ctx) {
    const t = 1 - this.life / this.maxLife;
    const alpha = t < 0.2 ? t / 0.2 : Math.max(0, 1 - (t - 0.6) / 0.4);
    const size = 14 + 4 * Math.sin(this.phase * 0.5);

    ctx.save();
    ctx.globalAlpha = alpha * 0.4;
    ctx.shadowColor = this.glowColor;
    ctx.shadowBlur = 12;
    ctx.font = `${Math.round(size + 4)}px "JetBrains Mono", monospace`;
    ctx.textAlign = "center";
    ctx.fillStyle = this.glowColor;
    ctx.fillText(this.glyph, this.x, this.y);

    ctx.globalAlpha = alpha;
    ctx.shadowBlur = 0;
    ctx.font = `${Math.round(size)}px "JetBrains Mono", monospace`;
    ctx.fillStyle = this.color;
    ctx.fillText(this.glyph, this.x, this.y);
    ctx.restore();
  }
}

// ─── SHOCKWAVE (expanding ring) ─────────────────────────────────────
class Shockwave {
  constructor(x, y, color, maxRadius = 60, duration = 0.8) {
    this.x = x;
    this.y = y;
    this.color = color;
    this.maxRadius = maxRadius;
    this.life = duration;
    this.maxLife = duration;
  }

  get alive() { return this.life > 0; }

  update(dt) { this.life -= dt; }

  render(ctx) {
    const t = 1 - this.life / this.maxLife;
    const r = this.maxRadius * t;
    const alpha = 1 - t;
    ctx.save();
    ctx.globalAlpha = alpha * 0.6;
    ctx.strokeStyle = this.color;
    ctx.lineWidth = 2 * (1 - t);
    ctx.beginPath();
    ctx.arc(this.x, this.y, r, 0, Math.PI * 2);
    ctx.stroke();
    ctx.restore();
  }
}

// ─── TELEPORT BEAM (vertical light column) ──────────────────────────
class TeleportBeam {
  constructor(x, y, color, duration = 1.2) {
    this.x = x;
    this.y = y;
    this.color = color;
    this.life = duration;
    this.maxLife = duration;
  }

  get alive() { return this.life > 0; }

  update(dt) { this.life -= dt; }

  render(ctx) {
    const t = 1 - this.life / this.maxLife;
    const alpha = t < 0.3 ? t / 0.3 : Math.max(0, 1 - (t - 0.5) / 0.5);
    const width = 6 + 10 * (1 - t);
    const height = 120 * Math.min(1, t * 3);

    ctx.save();
    ctx.globalAlpha = alpha * 0.5;

    // Beam gradient
    const grad = ctx.createLinearGradient(this.x, this.y - height, this.x, this.y + 10);
    grad.addColorStop(0, "transparent");
    grad.addColorStop(0.3, this.color);
    grad.addColorStop(0.7, this.color);
    grad.addColorStop(1, "transparent");

    ctx.fillStyle = grad;
    ctx.fillRect(this.x - width / 2, this.y - height, width, height + 10);

    // Core white line
    ctx.globalAlpha = alpha * 0.8;
    ctx.fillStyle = "#fff";
    ctx.fillRect(this.x - 1, this.y - height * 0.8, 2, height * 0.8);

    ctx.restore();
  }
}

// ─── PIXEL BURST (square particles flying out) ──────────────────────
class PixelBurst {
  constructor(x, y, color, count = 8, speed = 50, life = 0.6, gravity = 0) {
    this.particles = [];
    for (let i = 0; i < count; i++) {
      const angle = (Math.PI * 2 * i) / count + (Math.random() - 0.5) * 0.4;
      const spd = speed * (0.6 + Math.random() * 0.8);
      this.particles.push({
        x, y,
        vx: Math.cos(angle) * spd,
        vy: Math.sin(angle) * spd,
        life: life + Math.random() * 0.3,
        maxLife: life + Math.random() * 0.3,
        size: 2 + Math.random() * 2,
        color,
        gravity,
      });
    }
  }

  get alive() { return this.particles.some(p => p.life > 0); }

  update(dt) {
    for (const p of this.particles) {
      if (p.life <= 0) continue;
      p.life -= dt;
      p.x += p.vx * dt;
      p.y += p.vy * dt;
      p.vy += p.gravity * dt;
    }
  }

  render(ctx) {
    for (const p of this.particles) {
      if (p.life <= 0) continue;
      const alpha = p.life / p.maxLife;
      ctx.save();
      ctx.globalAlpha = alpha;
      ctx.fillStyle = p.color;
      ctx.fillRect(p.x - p.size / 2, p.y - p.size / 2, p.size, p.size);
      ctx.restore();
    }
  }
}

// ─── MCP EFFECTS ENGINE ─────────────────────────────────────────────
export class MCPEffects {
  constructor() {
    this.effects = [];
    this._eventSource = null;
    this._agentResolver = null; // function(project, name) => {x, y} or null
  }

  /** Set a function that resolves agent name → canvas position */
  setAgentResolver(fn) {
    this._agentResolver = fn;
  }

  /** Connect to the MCP events SSE stream */
  connect() {
    this._eventSource = new EventSource("/api/events/stream");
    this._eventSource.onmessage = (e) => {
      try {
        const evt = JSON.parse(e.data);
        this._handleEvent(evt);
      } catch (err) {
        console.error("[mcp-fx] parse error:", err);
      }
    };
    this._eventSource.onerror = () => {
      // Reconnects automatically via EventSource
    };
  }

  disconnect() {
    if (this._eventSource) {
      this._eventSource.close();
      this._eventSource = null;
    }
  }

  _resolve(project, name) {
    if (!this._agentResolver) return null;
    return this._agentResolver(project, name);
  }

  _handleEvent(evt) {
    const pos = this._resolve(evt.project, evt.agent);
    if (!pos) return; // agent not on screen

    switch (evt.type) {
      case "memory":
        this._memoryEffect(pos, evt);
        break;
      case "task":
        this._taskEffect(pos, evt);
        break;
      case "register":
        this._registerEffect(pos, evt);
        break;
    }
  }

  // ─── MEMORY EFFECTS ────────────────────────────────────────────
  _memoryEffect(pos, evt) {
    const { x, y } = pos;
    if (evt.action === "conflict") {
      // Amber warning flash + lightning glyph
      this.effects.push(new Shockwave(x, y - 20, COLORS.conflict.core, 45, 0.6));
      this.effects.push(new GlyphRise(x, y - 30, "⚡", COLORS.conflict.core, COLORS.conflict.glow));
      this.effects.push(new PixelBurst(x, y - 20, COLORS.conflict.accent, 6, 35, 0.5));
      this.effects.push(new FloatingText(x, y - 55, "CONFLICT", COLORS.conflict.core, 1.8));
    } else if (evt.action === "resolve") {
      // Green resolution glow
      this.effects.push(new Shockwave(x, y - 20, COLORS.resolve.core, 50, 0.8));
      this.effects.push(new GlyphRise(x, y - 30, "✓", COLORS.resolve.core, COLORS.resolve.glow));
      this.effects.push(new FloatingText(x, y - 55, "RESOLVED", COLORS.resolve.core, 1.5));
    } else {
      // set_memory — cyan crystal rising
      this.effects.push(new GlyphRise(x + (Math.random() - 0.5) * 10, y - 30, "◆", COLORS.memory.core, COLORS.memory.glow, 1.8));
      this.effects.push(new PixelBurst(x, y - 25, COLORS.memory.accent, 4, 20, 0.4, 0));
    }
  }

  // ─── TASK EFFECTS ──────────────────────────────────────────────
  _taskEffect(pos, evt) {
    const { x, y } = pos;
    switch (evt.action) {
      case "dispatch": {
        // Hot pink burst outward — task sent
        this.effects.push(new Shockwave(x, y, COLORS.task.core, 55, 0.7));
        this.effects.push(new PixelBurst(x, y - 10, COLORS.task.core, 10, 60, 0.7));
        this.effects.push(new FloatingText(x, y - 50, "DISPATCH", COLORS.task.core, 1.5));

        // If target agent is on screen, send a traveling orb
        if (evt.target) {
          const targetPos = this._resolve(evt.project, evt.target);
          if (targetPos) {
            this.effects.push(new Shockwave(targetPos.x, targetPos.y, COLORS.task.accent, 35, 0.6));
          }
        }
        break;
      }
      case "claim":
        // Quick intake flash
        this.effects.push(new Shockwave(x, y, COLORS.task.accent, 30, 0.4));
        this.effects.push(new FloatingText(x, y - 45, "CLAIMED", COLORS.task.core, 1.2));
        break;
      case "start":
        // Green pulse ring — work beginning
        this.effects.push(new Shockwave(x, y, COLORS.complete.core, 40, 0.6));
        this.effects.push(new FloatingText(x, y - 45, "STARTED", COLORS.complete.core, 1.2));
        break;
      case "complete":
        // Confetti burst + checkmark
        this.effects.push(new Shockwave(x, y - 10, COLORS.complete.core, 60, 0.8));
        this.effects.push(new GlyphRise(x, y - 30, "✓", COLORS.complete.core, COLORS.complete.glow, 2.0));
        this.effects.push(new PixelBurst(x, y - 10, COLORS.complete.core, 12, 70, 0.8, 40));
        this.effects.push(new FloatingText(x, y - 55, "DONE", COLORS.complete.core, 2.0));
        break;
      case "block":
        // Red shockwave + warning
        this.effects.push(new Shockwave(x, y, COLORS.block.core, 55, 0.6));
        this.effects.push(new Shockwave(x, y, COLORS.block.glow, 40, 0.4));
        this.effects.push(new GlyphRise(x, y - 30, "✕", COLORS.block.core, COLORS.block.glow, 2.0));
        this.effects.push(new PixelBurst(x, y - 10, COLORS.block.accent, 8, 50, 0.6));
        this.effects.push(new FloatingText(x, y - 55, "BLOCKED", COLORS.block.core, 2.0));
        break;
    }
  }

  // ─── REGISTER EFFECTS ─────────────────────────────────────────
  _registerEffect(pos, evt) {
    const { x, y } = pos;
    switch (evt.action) {
      case "register":
      case "respawn":
        // Teleport beam + spawn particles
        this.effects.push(new TeleportBeam(x, y, COLORS.register.core, 1.5));
        this.effects.push(new PixelBurst(x, y, COLORS.register.accent, 12, 45, 0.8, -20));
        this.effects.push(new Shockwave(x, y, COLORS.register.core, 50, 0.8));
        break;
      case "sleep":
        // Lavender fade with zzZ
        this.effects.push(new GlyphRise(x + 15, y - 25, "z", COLORS.sleep.core, COLORS.sleep.glow, 2.5));
        this.effects.push(new GlyphRise(x + 25, y - 35, "z", COLORS.sleep.accent, COLORS.sleep.glow, 3.0));
        this.effects.push(new GlyphRise(x + 35, y - 45, "Z", COLORS.sleep.core, COLORS.sleep.glow, 3.5));
        break;
      case "deactivate":
        // Dissolve into pixels
        this.effects.push(new PixelBurst(x, y, COLORS.sleep.core, 16, 30, 1.2, 15));
        this.effects.push(new FloatingText(x, y - 45, "OFFLINE", COLORS.sleep.core, 1.5));
        break;
    }
  }

  // ─── UPDATE & RENDER ──────────────────────────────────────────
  update(dt) {
    for (let i = this.effects.length - 1; i >= 0; i--) {
      this.effects[i].update(dt);
      if (!this.effects[i].alive) {
        this.effects.splice(i, 1);
      }
    }
  }

  render(ctx) {
    for (const fx of this.effects) {
      fx.render(ctx);
    }
  }
}
