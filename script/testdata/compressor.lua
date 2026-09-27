-- A feed-forward compressor: a detector tracks the signal's level, and the
-- softknee shaper reduces everything above the threshold by the ratio, easing
-- the corner over the knee width. Lookahead delays the audio so the detector
-- sees a transient before it arrives, and the effect reports that delay as its
-- latency so the play position stays honest.
return effect {
  name = "Compressor",
  params = {
    threshold = { float, min = -60, max = 0, default = -18, unit = "dB", label = "Threshold", group = "Compressor" },
    ratio     = { float, min = 1, max = 20, default = 4, label = "Ratio", group = "Compressor" },
    knee      = { float, min = 0, max = 24, default = 0, unit = "dB", label = "Knee", group = "Compressor" },
    attack    = { float, min = 0.0005, max = 0.5, default = 0.01, unit = "s", label = "Attack", group = "Compressor" },
    release   = { float, min = 0.01, max = 2, default = 0.1, unit = "s", label = "Release", group = "Compressor" },
    makeup    = { float, min = 0, max = 24, default = 0, unit = "dB", label = "Makeup", group = "Compressor" },
    mix       = { float, min = 0, max = 1, default = 1, label = "Mix", group = "Compressor" },
    lookahead = { float, min = 0, max = 0.02, default = 0, unit = "s", label = "Lookahead", group = "Compressor" },
  },
  build = function(ctx)
    local env  = ctx.env { attack = ctx.param("attack"),
                          release = ctx.param("release"), mode = "rms" }
    env.input = ctx.input()
    local db   = ctx.db(env)
    -- A level in dB in, a positive reduction in dB out; knee of zero is the
    -- hard knee.
    local red  = ctx.shaper { shape = "softknee",
                              threshold = ctx.param("threshold"),
                              ratio = ctx.param("ratio"),
                              knee = ctx.param("knee") }(db)
    local gr   = ctx.meter("gr", { label = "Gain Reduction", unit = "dB",
                                   min = -30, max = 0, kind = "gain-reduction" })
    gr(-red)

    -- The detector reads the dry input, so it sees the transient before the
    -- delayed wet path arrives.
    local dry   = ctx.delay { max = 0.02, time = ctx.param("lookahead") }
    dry.input   = ctx.input()

    local gain  = ctx.db2lin(ctx.param("makeup") - red)
    local wet   = dry * gain
    ctx.out(ctx.input() * (1 - ctx.param("mix")) + wet * ctx.param("mix"))

    ctx.latency(ctx.param("lookahead"))

    ctx.visual {
      kind = "transfer",
      params = { "threshold", "ratio", "makeup", "knee" },
      overlays = { "in", "gr" },
      xMin = -60, xMax = 0, yMin = -60, yMax = 0,
    }
    ctx.visual {
      kind = "dynamics",
      params = {},
      overlays = { "in", "out", "gr" },
      yMin = -60, yMax = 0,
    }
  end,
}
