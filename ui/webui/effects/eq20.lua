-- A 20-band graphic EQ: twenty peaking filters in series, log spaced from 25 Hz
-- to 16 kHz. Each band is one gain slider; frequency and Q are fixed, which is
-- what makes it graphic rather than parametric.
--
-- The keys are zero-padded (gain01..gain20) because the loader reads params in
-- sorted key order, so gain1/gain10/gain2 would scramble the slider order.
--
-- At 0 dB each peaking filter is unity (A = 1 makes b0 = a0), so flat is
-- transparent.

local BANDS = 20
local LOW = 25.0     -- lowest band centre, Hz
local HIGH = 16000.0 -- highest band centre, Hz; must stay under rate/2
local Q = 1.4

-- Log spacing, so every band covers the same interval in octaves.
local ratio = (HIGH / LOW) ^ (1 / (BANDS - 1))

local params = {}
local centers = {}
for i = 1, BANDS do
  local freq = LOW * ratio ^ (i - 1)
  centers[i] = freq
  params[string.format("gain%02d", i)] = {
    float,
    min = -12, max = 12, default = 0, step = 0.1,
    unit = "dB",
    label = string.format("%d Hz", math.floor(freq + 0.5)),
    group = "Bands",
    widget = "slider",
  }
end

return effect {
  name = "20-Band EQ",
  params = params,
  build = function(ctx)
    local sig = ctx.input()
    for i = 1, BANDS do
      local band = ctx.biquad {
        type = "peaking",
        freq = centers[i],
        q = Q,
        gain = ctx.param(string.format("gain%02d", i)),
      }
      sig = band(sig)
    end
    ctx.out(sig)
  end,
}
