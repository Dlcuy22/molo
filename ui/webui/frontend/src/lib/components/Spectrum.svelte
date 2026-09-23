<script lang="ts">
  import { onMount } from "svelte";
  import { onSpectrum } from "../store";

  // The visualizer brief: bar shape, 150 points by default, a 200px tall bar
  // area, 3.8px bars with rounded ends, and a display that re-ranges to the
  // loudest recent content.
  const HEIGHT = 200;
  const LINE_WIDTH = 3.8;

  // Debug overlay: the canvas redraw rate, the display's refresh rate, and the
  // average gap between incoming spectrum frames. A canvas that could redraw at
  // 60 while frames arrive every 90 ms is exactly what "choppy but the FPS
  // looks fine" means, so the three numbers are shown side by side. Visible on
  // launch; the D key hides it.
  let showDebug = $state(true);
  let drawFps = $state(0);
  let rafFps = $state(0);
  let gapMs = $state(0);
  let measured = $state(false);

  let canvas: HTMLCanvasElement;

  onMount(() => {
    const ctx = canvas.getContext("2d");
    if (!ctx) {
      return;
    }

    // Colours come from the design tokens rather than being duplicated here, so
    // the visualizer cannot drift from the palette.
    const styles = getComputedStyle(document.documentElement);
    const barColor = styles.getPropertyValue("--color-fg").trim() || "#f2f0ea";

    // The buffer is sized to the backing store on resize, so a HiDPI display
    // draws crisp bars instead of a scaled-up blur. The CSS size is cached
    // here so a frame never reads layout: a clientWidth read per event would
    // force a reflow 30 times a second interleaved with Svelte's DOM writes.
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
      drawnSeq = -1; // a new size must redraw even with no new frame
    };

    // Frames arrive over the bridge at up to 60 Hz on the IPC task queue, at
    // arbitrary phase to the display. Drawing each arrival immediately would
    // judder whenever events bunch up, so arrivals only publish the latest
    // frame and one rAF loop below draws it at most once per vsync. A hidden
    // tab draws nothing at all: rAF does not fire there.
    let latest: Float64Array | null = null;
    let seq = 0;
    let drawnSeq = -1;

    // Counters behind the debug overlay. The rAF rate is the display's own
    // refresh rate; the draw rate counts frames actually painted, which is
    // capped by how often new spectrum frames arrive. The gap is the wall
    // clock spacing between arrivals, averaged over the same second: with the
    // runner's jitter buffer the pump should deliver near the 16 ms display
    // interval, so a gap approaching the device pull is the choppiness coming
    // back.
    let renders = 0;
    let rafs = 0;
    let arrivals = 0;
    let gapSum = 0;
    let gapCount = 0;
    let lastArrival = 0;

    const unsubFrames = onSpectrum((bands) => {
      const now = performance.now();
      if (lastArrival > 0) {
        gapSum += now - lastArrival;
        gapCount++;
      }
      lastArrival = now;
      arrivals++;
      latest = bands;
      seq++;
    });

    resize();
    const observer = new ResizeObserver(resize);
    observer.observe(canvas);

    let raf = 0;
    let lastStats = 0;
    const draw = () => {
      raf = requestAnimationFrame(draw);

      const now = performance.now();
      rafs++;
      if (now - lastStats >= 1000) {
        rafFps = Math.round((rafs * 1000) / (now - lastStats));
        drawFps = Math.round((renders * 1000) / (now - lastStats));
        // Retain the last measured gap when nothing arrived this second
        // (paused, stopped), so the number stays meaningful instead of
        // collapsing to zero.
        if (gapCount > 0) {
          gapMs = Math.round(gapSum / gapCount);
        }
        measured = true;
        renders = 0;
        rafs = 0;
        arrivals = 0;
        gapSum = 0;
        gapCount = 0;
        lastStats = now;
      }

      // The backend suppresses repeats (a held pause frame, drained silence),
      // so usually there is nothing new and this returns without touching
      // the canvas.
      if (latest === null || seq === drawnSeq) {
        return;
      }
      drawnSeq = seq;
      renders++;

      const bands = latest;
      const w = cssW;
      const h = cssH;
      ctx.clearRect(0, 0, w, h);
      if (bands.length === 0 || w <= 0 || h <= 0) {
        return;
      }

      const n = bands.length;
      // Bars are spaced by the point count and drawn at the configured width.
      // When the strip is too narrow for that width the bars shrink rather than
      // overlap, so a squeezed window still reads as a spectrum.
      const slot = w / n;
      const width = Math.min(LINE_WIDTH, Math.max(1, slot * 0.8));

      // A round cap makes each bar a rounded-ended stroke, which is the
      // "rounded corners" and "bar border" of the brief: the stroke is the
      // bar's edge, so there is no separate outline to double up.
      ctx.lineCap = "round";
      ctx.lineJoin = "round";
      ctx.lineWidth = width;
      ctx.strokeStyle = barColor;

      // The 200px band is the drawing area. A bar is at least the line width
      // tall so a silent band still draws a visible floor dot rather than
      // vanishing, which is what makes an idle player look running.
      const base = width / 2;

      for (let i = 0; i < n; i++) {
        const level = Math.min(1, Math.max(0, bands[i]));
        const x = slot * (i + 0.5);
        const barH = Math.max(width, level * (h - width));
        const y = h - base - barH + width / 2;

        // Opacity tracks the level as well as the height, so a quiet band is
        // dim rather than a short bright stub.
        ctx.globalAlpha = 0.35 + level * 0.65;
        ctx.beginPath();
        ctx.moveTo(x, h - base);
        ctx.lineTo(x, y);
        ctx.stroke();
      }
      ctx.globalAlpha = 1;
    };
    raf = requestAnimationFrame(draw);

    // The D key toggles the overlay, which stays out of the way of every
    // other key the app uses.
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "d" || e.key === "D") {
        showDebug = !showDebug;
      }
    };
    window.addEventListener("keydown", onKey);

    return () => {
      cancelAnimationFrame(raf);
      unsubFrames();
      observer.disconnect();
      window.removeEventListener("keydown", onKey);
    };
  });
</script>

<!--
  The spectrum is the window's widest element because the brief's 150 points at
  3.8px need width to read as bars rather than a smear. It is the one place the
  signal colour is white; the accent stays reserved for playback.
-->
<div class="rounded-[6px] border border-line bg-bg">
  <!-- The canvas is decorative to a screen reader: it carries no information
       the status line does not. -->
  <canvas bind:this={canvas} class="block w-full" style="height: {HEIGHT}px" aria-hidden="true"
  ></canvas>
</div>
{#if showDebug}
  <!-- A dev readout, not product chrome: the numbers exist to tell a smooth
       display from a fast one, and D hides them. -->
  <div
    class="rounded-[4px] border border-line bg-surface px-2 py-1 font-mono text-[11px] tabular-nums text-muted"
    role="status"
  >
    {measured ? drawFps : "--"} fps draw &middot; {measured ? rafFps : "--"} fps vsync &middot; {measured
      ? gapMs
      : "--"} ms frame gap
  </div>
{/if}
<p class="sr-only">Live audio spectrum, updated while a track plays.</p>
