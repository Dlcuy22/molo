<script lang="ts">
  import { untrack } from "svelte";
  import { onEffectMeters } from "../../store";
  import type { Reading, Visual } from "../../effect-types";
  import {
    createRing,
    meterValue,
    plotX,
    push,
    resolveSeries,
    series,
    levelToY,
    type Ring,
    type SeriesInfo,
  } from "./dynamics-graph";

  // A scrolling three-layer dynamics graph: a level fills from the floor, a
  // gain reduction hangs below 0, a scalar draws a line. The series come from
  // the fast meter tick, so the graph moves at the meter rate and not at the
  // 4 Hz snapshot. The effect declares the overlays and their readings; the
  // renderer lives here.
  let {
    visual,
    readings,
    stageId,
    meters,
  }: {
    visual: Visual;
    readings: Reading[];
    stageId: string;
    meters: Record<string, number> | null;
  } = $props();

  // ~3 seconds at 60 Hz. The window is fixed by the display rate, not by the
  // panel width, so a wide panel shows the same span as a narrow one.
  const CAPACITY = 180;
  const HEIGHT = 132;
  // The meter strip is a fixed gutter so the plot loses width but never
  // overlaps it; it carries output level and gain reduction at a glance.
  const STRIP_W = 14;
  const STRIP_GAP = 8;

  const infos = $derived(resolveSeries(visual.overlays, readings));

  // The graph's identity as a string. The panel rebuilds `readings` on every
  // meter tick, so a plain `infos` dependency would restart the draw loop 60
  // times a second and no frame would ever land. This changes only when the
  // stage, the overlay set, their kinds, or the scale actually change.
  const signature = $derived(
    JSON.stringify({
      stage: stageId,
      overlays: visual.overlays,
      kinds: infos.map((s) => `${s.key}:${s.kind}`),
      x: [visual.xMin, visual.xMax],
      y: [visual.yMin, visual.yMax],
    }),
  );

  // The stated legend, not the live values: this is what the graph is, so it
  // reads the same at rest as while playing.
  const summary = $derived(
    infos.length === 0
      ? "Dynamics graph: no series declared for this effect."
      : `Dynamics graph, newest on the right: ${infos.map((s) => `${s.key} (${s.kind})`).join(", ")}.`,
  );

  let canvas: HTMLCanvasElement;

  // Re-runs when the graph's identity changes. EffectPanel mounts this inside
  // an each keyed by index, so a stage switch reuses the instance with a new
  // stageId and visual; without a rebuild the subscription and rings would stay
  // pointed at the old stage. `signature` is the only reactive read, so the
  // 60 Hz meter tick does not restart the loop.
  $effect(() => {
    void signature;
    const stage = untrack(() => stageId);
    const info = untrack(() => infos);
    // Every prop read is untracked: the panel rebuilds `visuals` on each
    // snapshot, so a tracked read would restart the loop and empty the rings.
    // `signature` is what decides a rebuild.
    const scale = untrack(() => visual.yMin);
    const ceiling = untrack(() => visual.yMax);
    const xMin = untrack(() => visual.xMin);
    const xMax = untrack(() => visual.xMax);
    // A bad range disables the plot rather than collapsing it onto an edge.
    const valid =
      Number.isFinite(scale) &&
      Number.isFinite(ceiling) &&
      Number.isFinite(xMin) &&
      Number.isFinite(xMax) &&
      ceiling > scale;
    // Seeding from the current meters avoids a frame of empty plot on setup.
    const seed = untrack(() => meters);

    const ctx = canvas.getContext("2d");
    if (!ctx) {
      return;
    }

    const styles = getComputedStyle(document.documentElement);
    const token = (name: string, fallback: string) =>
      styles.getPropertyValue(name).trim() || fallback;
    // Colours come from the design tokens so the graph cannot drift from the
    // palette; the literals only cover an empty token.
    const colors = {
      fg: token("--color-fg", "#f2f0ea"),
      muted: token("--color-muted", "#b0aca2"),
      accent: token("--color-accent", "#3584e4"),
      line: token("--color-line", "#454548"),
      danger: token("--color-danger", "#c44536"),
    };

    const rings = new Map<string, Ring>();
    for (const s of info) {
      rings.set(s.key, createRing(CAPACITY));
    }
    let latestMeters: Record<string, number> | null = seed;

    // Frames arrive on the meter tick, which is not phase-locked to the
    // display, so a push only publishes a new sequence and one rAF loop draws
    // at most once per vsync. A hidden tab draws nothing.
    let seq = 0;
    let drawnSeq = -1;

    const pushLatest = (m: Record<string, number> | null) => {
      for (const s of info) {
        // An absent or non-finite meter is a gap, not a zero: inventing a value
        // would draw silence the effect never published.
        push(rings.get(s.key)!, meterValue(m, s.key));
      }
      seq++;
    };
    pushLatest(latestMeters);

    const unsub = onEffectMeters((rows) => {
      const row = rows.find((r) => r.id === stage);
      if (!row) {
        return;
      }
      latestMeters = row.meters;
      pushLatest(row.meters);
    });

    let cssW = 0;
    let cssH = 0;
    const resize = () => {
      const dpr = window.devicePixelRatio || 1;
      const rect = canvas.getBoundingClientRect();
      cssW = rect.width;
      cssH = rect.height;
      canvas.width = Math.max(1, Math.round(rect.width * dpr));
      canvas.height = Math.max(1, Math.round(rect.height * dpr));
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      drawnSeq = -1; // a new size must redraw even with no new sample
    };
    resize();
    const observer = new ResizeObserver(resize);
    observer.observe(canvas);

    const drawStrips = (w: number, h: number) => {
      const level = meterValue(latestMeters, "out");
      const gr = meterValue(latestMeters, "gr");
      const top = levelToY(0, scale, ceiling, h) ?? 0;
      const x0 = w - STRIP_W;

      // Output level fills upward from the floor to the current value.
      ctx.strokeStyle = colors.line;
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(x0 - 0.5, 0);
      ctx.lineTo(x0 - 0.5, h);
      ctx.stroke();

      ctx.fillStyle = colors.accent;
      ctx.beginPath();
      ctx.rect(x0, top, STRIP_W, Math.max(0, h - top));
      ctx.fill();

      // Gain reduction fills downward from the 0 line to the current value,
      // which is the same dB scale the plot uses.
      const grY = gr === null ? null : levelToY(gr, scale, ceiling, h);
      if (grY !== null) {
        ctx.fillStyle = colors.danger;
        ctx.globalAlpha = 0.85;
        ctx.beginPath();
        ctx.rect(x0, top, STRIP_W, Math.max(0, grY - top));
        ctx.fill();
        ctx.globalAlpha = 1;
      }
    };

    const drawGrid = (w: number, h: number) => {
      ctx.lineWidth = 1;
      ctx.font = "9px system-ui, sans-serif";
      ctx.textBaseline = "middle";
      // A muted line every 12 dB with its value at the left, plus a stronger 0
      // line, so a level can be read off the plot rather than only felt.
      for (let db = Math.ceil(scale / 12) * 12; db <= ceiling; db += 12) {
        const y = levelToY(db, scale, ceiling, h);
        if (y === null) {
          continue;
        }
        ctx.strokeStyle = colors.line;
        ctx.globalAlpha = db === 0 ? 0.9 : 0.4;
        ctx.beginPath();
        ctx.moveTo(0, y + 0.5);
        ctx.lineTo(w, y + 0.5);
        ctx.stroke();
        ctx.globalAlpha = 1;
        ctx.fillStyle = colors.muted;
        ctx.fillText(String(db), 2, y);
      }
    };

    const drawSeries = (s: SeriesInfo, data: (number | null)[], w: number, h: number) => {
      if (data.length === 0) {
        return;
      }
      const xAt = (i: number) => plotX(i, data.length, w, CAPACITY);
      const yAt = (v: number) => levelToY(v, scale, ceiling, h);

      if (s.kind === "level") {
        // A filled area from the floor up to the value. The path is split on
        // holes so a gap stays a gap rather than a line through the missing
        // span; each contiguous run is filled on its own.
        const floor = levelToY(scale, scale, ceiling, h) ?? h;
        // "out" is the reference level, so it gets the accent the eye should
        // follow; any other level stays in the foreground colour.
        ctx.fillStyle = s.key === "out" ? colors.accent : colors.fg;
        ctx.globalAlpha = 0.28;
        let run: { x: number; y: number }[] = [];
        const flush = () => {
          if (run.length >= 2) {
            ctx.beginPath();
            ctx.moveTo(run[0].x, floor);
            for (const p of run) {
              ctx.lineTo(p.x, p.y);
            }
            ctx.lineTo(run[run.length - 1].x, floor);
            ctx.closePath();
            ctx.fill();
          }
          run = [];
        };
        for (let i = 0; i < data.length; i++) {
          const v = data[i];
          const y = v === null ? null : yAt(v);
          if (y === null) {
            flush();
            continue;
          }
          run.push({ x: xAt(i), y });
        }
        flush();
        ctx.globalAlpha = 1;

        return;
      }

      // A gain reduction sits at its (negative) value on the same dB scale, so
      // it hangs below the 0 line; a scalar is the same polyline in a different
      // colour. Both skip holes.
      // The reduction takes danger because it is the one line that means the
      // signal is being pulled down; a scalar rides the accent.
      ctx.strokeStyle = s.kind === "gain-reduction" ? colors.danger : colors.accent;
      ctx.lineWidth = 1.5;
      ctx.lineJoin = "round";
      ctx.beginPath();
      let pen = false;
      for (let i = 0; i < data.length; i++) {
        const v = data[i];
        const y = v === null ? null : yAt(v);
        if (y === null) {
          pen = false;
          continue;
        }
        const x = xAt(i);
        if (pen) {
          ctx.lineTo(x, y);
        } else {
          ctx.moveTo(x, y);
          pen = true;
        }
      }
      ctx.stroke();
    };

    let raf = 0;
    const draw = () => {
      raf = requestAnimationFrame(draw);
      if (seq === drawnSeq) {
        return;
      }
      drawnSeq = seq;

      const w = cssW;
      const h = cssH;
      ctx.clearRect(0, 0, w, h);
      if (w <= 0 || h <= 0) {
        return;
      }

      const plotW = Math.max(0, w - STRIP_W - STRIP_GAP);
      if (valid) {
        drawGrid(plotW, h);
        for (const s of info) {
          const ring = rings.get(s.key);
          if (ring) {
            ctx.save();
            ctx.beginPath();
            ctx.rect(0, 0, plotW, h);
            ctx.clip();
            drawSeries(s, series(ring), plotW, h);
            ctx.restore();
          }
        }
      }
      drawStrips(w, h);
    };
    raf = requestAnimationFrame(draw);

    return () => {
      cancelAnimationFrame(raf);
      unsub();
      observer.disconnect();
    };
  });
</script>

<!--
  The graph fills the panel's width and keeps a fixed height, so the effect
  window's scroll never jumps as it runs. The canvas is decorative to a screen
  reader; the summary paragraph below carries the same information in words.
-->
<div class="rounded-[6px] border border-line bg-bg">
  <canvas
    bind:this={canvas}
    class="block w-full"
    style="height: {HEIGHT}px"
    aria-hidden="true"
  ></canvas>
</div>
<p class="sr-only">{summary}</p>
