-- A feed-forward compressor: a detector tracks the signal's level, everything
-- above the threshold is reduced by the ratio, and the effect reports the gain
-- reduction and the transfer curve it applies. The law is hard-knee: the node
-- vocabulary has no conditional or power, so a soft knee cannot be expressed
-- here, and a Knee knob that changed nothing would be a lie.
return effect {
  name = "Compressor",
  params = {
    threshold = { float, min = -60, max = 0, default = -18, unit = "dB", label = "Threshold", group = "Compressor" },
    ratio     = { float, min = 1, max = 20, default = 4, label = "Ratio", group = "Compressor" },
    attack    = { float, min = 0.0005, max = 0.5, default = 0.01, unit = "s", label = "Attack", group = "Compressor" },
    release   = { float, min = 0.01, max = 2, default = 0.1, unit = "s", label = "Release", group = "Compressor" },
    makeup    = { float, min = 0, max = 24, default = 0, unit = "dB", label = "Makeup", group = "Compressor" },
    mix       = { float, min = 0, max = 1, default = 1, label = "Mix", group = "Compressor" },
  },
  build = function(ctx)
    local env  = ctx.env { attack = ctx.param("attack"),
                          release = ctx.param("release"), mode = "rms" }
    env.input = ctx.input()
    local db   = ctx.db(env)
    local over = ctx.max(ctx.const(0), db - ctx.param("threshold"))
    -- Reduction in dB, positive; the meter reports it as a negative gain.
    local red  = over * (1 - 1 / ctx.param("ratio"))
    local gr   = ctx.meter("gr", { label = "Gain Reduction", unit = "dB",
                                   min = -30, max = 0, kind = "gain-reduction" })
    gr(-red)

    -- Makeup lifts the whole output; the reduction is taken before it so the
    -- reading is the compressor's own action, not the trim.
    local gain  = ctx.db2lin(ctx.param("makeup") - red)
    local wet   = ctx.input() * gain
    ctx.out(ctx.input() * (1 - ctx.param("mix")) + wet * ctx.param("mix"))

    ctx.visual {
      kind = "transfer",
      params = { "threshold", "ratio", "makeup" },
      overlays = { "in", "gr" },
      xMin = -60, xMax = 0, yMin = -60, yMax = 0,
    }
  end,
}
