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

  // How many segments the path is sampled at. A curve is smooth, so a fixed
  // count that reads clean at panel width is enough; sampling more would only
  // cost path length without a visible difference.
  const STEPS = 64;

  // The plot's internal coordinate system. The SVG scales to the container, so
  // these are a convenient 0..1 range rather than pixels, and the component
  // reflows because the viewBox does the work.
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

  // dB to viewBox coordinates. The y axis is inverted because a louder level
  // is higher on the plot but a larger dBFS sits lower on screen. A zero or
  // non-finite span has no defined direction, so it falls back to 0 rather than
  // emitting NaN or an infinity into the path.
  function px(x: number): number {
    return xUsable ? (x - visual.xMin) / spanX : 0;
  }
  function py(y: number): number {
    return yUsable ? 1 - (y - visual.yMin) / spanY : 0;
  }

  // A bad range draws nothing at all: an empty path is honest about having no
  // scale, where a collapsed path would claim a shape the numbers do not have.
  const path = $derived(
    !rangeOK || !xUsable || !yUsable
      ? ""
      : points
          .map((p, i) => `${i === 0 ? "M" : "L"}${px(p.x).toFixed(4)} ${py(p.y).toFixed(4)}`)
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
    class="h-36 w-full rounded-[4px] border border-line bg-bg/40"
    viewBox="-0.06 -0.06 1.12 1.12"
    preserveAspectRatio="none"
    role="img"
    aria-label={label}
  >
    <!-- The axes are the scale, not decoration: without them the curve has no
         readable level. They are muted so the curve stays the one accent. -->
    <g class="text-muted" stroke="currentColor" stroke-width="1" opacity="0.5">
      <line x1="0" y1="0" x2="0" y2="1" vector-effect="non-scaling-stroke" />
      <line x1="0" y1="1" x2="1" y2="1" vector-effect="non-scaling-stroke" />
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
        stroke-width="1"
        stroke-dasharray="4 4"
        vector-effect="non-scaling-stroke"
      />
    {/if}

    <path
      d={path}
      fill="none"
      class="text-accent"
      stroke="currentColor"
      stroke-width="2"
      stroke-linejoin="round"
      stroke-linecap="round"
      vector-effect="non-scaling-stroke"
    />

    {#if rangeOK && xUsable && yUsable && liveIn !== null && liveOut !== null}
      <!-- The drop from the input axis up to the curve is the gain reduction
           the current signal is seeing. -->
      <line
        x1={px(liveIn)}
        y1="1"
        x2={px(liveIn)}
        y2={py(liveOut)}
        class="text-accent"
        stroke="currentColor"
        stroke-width="1"
        opacity="0.5"
        vector-effect="non-scaling-stroke"
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
          stroke-width="3"
          opacity="0.7"
          vector-effect="non-scaling-stroke"
        />
      {/if}
      <!-- Drawn as a zero-length round-capped stroke rather than a circle: the
           plot scales x and y independently, so a circle radius would render
           as an ellipse. A round cap under non-scaling-stroke stays round. -->
      <line
        x1={px(liveIn)}
        y1={py(liveOut)}
        x2={px(liveIn)}
        y2={py(liveOut)}
        class="text-accent"
        stroke="currentColor"
        stroke-width="7"
        stroke-linecap="round"
        vector-effect="non-scaling-stroke"
      />
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
