// Package playback turns a Provider of interleaved float32 frames into sound
// through a named backend.
//
// A backend is added by writing one file that implements Device plus an init
// that calls Register; the registry and this file never change. The first
// backend is oto; a second one (malgo) is expected to arrive the same way.
//
// The package models a push audio API as a pull: the backend owns the thread
// that asks for PCM, and the adapter in this package hides that inversion so
// every Device sees the same Provider contract as the rest of the engine.
package playback
