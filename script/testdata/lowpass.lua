return effect {
  name = "Lowpass",
  params = {
    freq = { float, min = 20, max = 20000, default = 1000, unit = "Hz", label = "Cutoff", group = "Filter", widget = "knob" },
    q    = { float, min = 0.1, max = 10, default = 0.707, label = "Q", group = "Filter", widget = "knob" },
  },
  build = function(ctx)
    local lp = ctx.biquad { type = "lowpass", freq = ctx.param("freq"), q = ctx.param("q") }
    ctx.out(lp(ctx.input()))
  end,
}
