<script lang="ts">
  import type { Visual } from "../../effect-types";
  import {
    gainReduction,
    resolveParams,
    transferLabel,
    transferPoints,
    transferSample,
  } from "./transfer-curve";

  // A compressor transfer plot: input level to output level, drawn from the
  // params the visual names. The effect declares the kind and the keys; the
  // renderer lives here, so a script author never writes SVG.
  let {
    visual,
    values,
    meters = null,
  }: {
    visual: Visual;
    values: Record<string, unknown>;
    meters?: Record<string, number> | null;
  } = $props();

  // The plot's coordinate system is a fixed box, not a unit square. It used to
  // be a unit square stretched with preserveAspectRatio="none" and every stroke
  // carried vector-effect="non-scaling-stroke"; WebKitGTK does not apply that
  // attribute, so a stroke-width of 2 in a 1-unit box blew up to fill the whole
  // plot as a solid rectangle. A fixed box with the CSS aspect-ratio below
  // scales x and y equally, so strokes keep their width with no vector-effect at
  // all. The box is 25:6, which is the old full-width h-36 shape at the effect
  // window's width.
  const VB_W = 1000;
  const VB_H = 240;
  const PAD = 18;

  // How many segments the path is sampled at. A curve is smooth, so a fixed
  // count that reads clean at panel width is enough; sampling more would only
  // cost path length without a visible difference.
  const STEPS = 64;

  const params = $derived(resolveParams(values, visual.params));

  // The curve is static: it is a function of the params, not the signal. It is
  // recomputed only when a param changes, which is what keeps it off the meter
  // path entirely.
  const points = $derived(transferPoints(params, visual.xMin, visual.xMax, STEPS));

  // A range is only usable when both ends are finite and differ. A non-finite
  // end is as degenerate as a zero span: either one would collapse the curve
  // onto a single edge, which reads as a flat line rather than as no data.
  const rangeOK = $derived(
    Number.isFinite(visual.xMin) &&
      Number.isFinite(visual.xMax) &&
      Number.isFinite(visual.yMin) &&
      Number.isFinite(visual.yMax),
  );
  const spanX = $derived(visual.xMax - visual.xMin);
  const spanY = $derived(visual.yMax - visual.yMin);
  const xUsable = $derived(Number.isFinite(spanX) && spanX !== 0);
  const yUsable = $derived(Number.isFinite(spanY) && spanY !== 0);

  // dB to viewBox coordinates. The y axis is inverted because a louder level is
  // higher on the plot but a larger dBFS sits lower on screen. A zero or
  // non-finite span has no defined direction, so it falls back to the padding
  // rather than emitting NaN or an infinity into the path.
  function px(x: number): number {
    return xUsable ? PAD + ((x - visual.xMin) / spanX) * (VB_W - 2 * PAD) : PAD;
  }
  function py(y: number): number {
    return yUsable ? PAD + (1 - (y - visual.yMin) / spanY) * (VB_H - 2 * PAD) : PAD;
  }

  // A bad range draws nothing at all: an empty path is honest about having no
  // scale, where a collapsed path would claim a shape the numbers do not have.
  const path = $derived(
    !rangeOK || !xUsable || !yUsable
      ? ""
      : points
          .map((p, i) => `${i === 0 ? "M" : "L"}${px(p.x).toFixed(2)} ${py(p.y).toFixed(2)}`)
          .join(" "),
  );

  // The live dot sits where the current input lands on the curve. It is the one
  // moving element on the plot, and it is only drawn when the effect actually
  // meters an input: a missing reading must not read as silence.
  const liveIn = $derived(
    meters !== null && typeof meters["in"] === "number" && Number.isFinite(meters["in"])
      ? meters["in"]
      : null,
  );
  const liveOut = $derived(liveIn === null ? null : transferSample(liveIn, params));

  const showGR = $derived(visual.overlays.includes("gr"));
  const liveGR = $derived(
    showGR && meters !== null && typeof meters["gr"] === "number" && Number.isFinite(meters["gr"])
      ? meters["gr"]
      : null,
  );
  // The law's own reduction at the live input, used when the effect does not
  // publish a gr reading of its own. A compressor's reduction is a property of
  // the curve, so the plot can show it without a second meter.
  const lawGR = $derived(liveIn === null ? null : gainReduction(liveIn, params));
  const grValue = $derived(liveGR ?? lawGR);

  const label = $derived(`Compressor transfer curve: ${transferLabel(params)}`);

  function fmt(n: number): string {
    return `${n >= 0 ? "+" : ""}${n.toFixed(1)} dB`;
  }
</script>

<!--
  The plot is a data display, so it is exposed as an image with a name rather
  than hidden as decoration: a screen reader gets the shape in words. Colour is
  not the only carrier either; the live values are printed as text beside it.
-->
<figure class="flex flex-col gap-1.5">
  <svg
    class="block w-full rounded-[4px] border border-line bg-bg/40"
    style="aspect-ratio: {VB_W} / {VB_H}"
    viewBox="0 0 {VB_W} {VB_H}"
    role="img"
    aria-label={label}
  >
    <!-- The axes are the scale, not decoration: without them the curve has no
         readable level. They are muted so the curve stays the one accent. -->
    <g class="text-muted" stroke="currentColor" stroke-width="2" opacity="0.5">
      <line x1={PAD} y1={PAD} x2={PAD} y2={VB_H - PAD} />
      <line x1={PAD} y1={VB_H - PAD} x2={VB_W - PAD} y2={VB_H - PAD} />
    </g>

    {#if rangeOK && xUsable && yUsable}
      <!-- The unity diagonal, so the knee and the compressed slope are read
           against the uncompressed reference. -->
      <line
        x1={px(visual.xMin)}
        y1={py(visual.xMin)}
        x2={px(visual.xMax)}
        y2={py(visual.xMax)}
        class="text-line"
        stroke="currentColor"
        stroke-width="2"
        stroke-dasharray="8 8"
      />
    {/if}

    <path
      d={path}
      fill="none"
      class="text-accent"
      stroke="currentColor"
      stroke-width="3"
      stroke-linejoin="round"
      stroke-linecap="round"
    />

    {#if rangeOK && xUsable && yUsable && liveIn !== null && liveOut !== null}
      <!-- The drop from the input axis up to the curve is the gain reduction
           the current signal is seeing. -->
      <line
        x1={px(liveIn)}
        y1={VB_H - PAD}
        x2={px(liveIn)}
        y2={py(liveOut)}
        class="text-accent"
        stroke="currentColor"
        stroke-width="2"
        opacity="0.5"
      />
      {#if grValue !== null && grValue < 0}
        <!-- The reduction marker: the vertical gap between the dot and the
             uncompressed diagonal at the same input, so its length is the dB
             the curve is pulling down. -->
        <line
          x1={px(liveIn)}
          y1={py(liveOut)}
          x2={px(liveIn)}
          y2={py(liveIn)}
          class="text-danger"
          stroke="currentColor"
          stroke-width="5"
          opacity="0.7"
        />
      {/if}
      <!-- A real circle, not a zero-length stroke: with a fixed box the scale is
           uniform, so a circle stays round and needs no vector-effect. -->
      <circle cx={px(liveIn)} cy={py(liveOut)} r="7" class="text-accent" fill="currentColor" />
    {/if}
  </svg>

  <!-- The live readout in words. The reduction is shown as a signed number so
       the value does not depend on the dot or on colour alone. -->
  <figcaption class="flex items-center justify-between gap-2 text-[11px] text-muted">
    <span>In to out, dB</span>
    {#if liveIn !== null}
      <span class="tabular-nums text-fg">
        {#if grValue !== null}
          {fmt(grValue)}
        {:else}
          in {fmt(liveIn)}
        {/if}
      </span>
    {:else}
      <span class="tabular-nums">{transferLabel(params)}</span>
    {/if}
  </figcaption>
</figure>
