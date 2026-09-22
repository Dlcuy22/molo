// Package dsp holds the real-time stages that run after the ring: gain,
// effects, limiting, metering. Everything in this package runs on the audio
// path, so a stage must not allocate, must not block, and must not take a lock
// a control goroutine can hold while waiting on the audio thread.
//
// Gain is the first such stage. The effect framework beside it follows the
// same shape as decode's codec registry: an effect is registered under a kind
// and an implementation name, a preset names the kind, and the highest-weight
// implementation wins the automatic choice. What differs is that an effect is
// editable while audio is running, so its parameters are published by an
// atomic swap instead of written in place.
package dsp
