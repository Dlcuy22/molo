// Package dsp holds the real-time stages that run after the ring: gain,
// limiting, metering. Everything in this package runs on the audio path, so
// modules must not allocate, must not block, and must not take locks that a
// control goroutine can hold while waiting on the audio thread.
//
// Gain is the first such module. It exists as its own layer, separate from the
// streamer, because volume must change while audio is already flowing.
package dsp
