return effect {
  name = "Delay",
  params = {
    time     = { float, min = 0.01, max = 1.5, default = 0.3, unit = "s", label = "Time", group = "Delay" },
    feedback = { float, min = 0, max = 0.95, default = 0.4, label = "Feedback", group = "Delay" },
    mix      = { float, min = 0, max = 1, default = 0.5, label = "Mix", group = "Delay" },
  },
  build = function(ctx)
    -- The line size is fixed by `max`; `time` only moves the read position, so
    -- a slider cannot reallocate the buffer on the audio thread.
    local d = ctx.delay { max = 1.5, time = ctx.param("time") }
    d.input = ctx.input() + d.tap * ctx.param("feedback")
    ctx.out(ctx.input() * (1 - ctx.param("mix")) + d.tap * ctx.param("mix"))
  end,
}
