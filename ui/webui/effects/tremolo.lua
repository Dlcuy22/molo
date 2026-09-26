return effect {
  name = "Tremolo",
  params = {
    rate  = { float, min = 0.1, max = 20, default = 5, unit = "Hz", label = "Rate", group = "Tremolo" },
    depth = { float, min = 0, max = 1, default = 0.5, label = "Depth", group = "Tremolo" },
  },
  build = function(ctx)
    -- depth 0 leaves the level at 1; depth 1 swings between 0 and 1.
    local lfo = ctx.lfo { rate = ctx.param("rate") }
    local amt = 1 - ctx.param("depth") * (lfo + 1) * 0.5
    ctx.out(ctx.input() * amt)
  end,
}
