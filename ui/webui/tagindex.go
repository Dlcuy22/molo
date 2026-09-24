package main

import (
	"sync"

	"github.com/dlcuy22/player/meta"
	"github.com/dlcuy22/player/ui/webui/internal/cover"
)

// tagIndexWorkers bounds how many tag readers run at once. Tag parsing opens
// the file and reads its header, so a folder of thousands must not open every
// file at the same moment.
const tagIndexWorkers = 4

// trackTags is the searchable identity of one queued track: the tags the meta
// chain resolved, empty when the file carried none. CoverID identifies the
// embedded art by content, so the palette can request the bytes lazily and the
// cache can key on it; it is empty when the track has no art.
type trackTags struct {
	Title   string
	Artist  string
	Album   string
	CoverID string
}

// tagIndex resolves the tags of queued tracks off the control path and caches
// them by path, so the palette can search a queue without the engine having to
// describe every track. Only the current queue is kept: a replaced queue drops
// the entries that are no longer in it.
type tagIndex struct {
	mu      sync.Mutex
	entries map[string]trackTags
	pending map[string]struct{}
	sem     chan struct{}
}

func newTagIndex() *tagIndex {
	return &tagIndex{
		entries: make(map[string]trackTags),
		pending: make(map[string]struct{}),
		sem:     make(chan struct{}, tagIndexWorkers),
	}
}

// lookup returns the cached tags for a path, or the zero value when it has not
// resolved yet. The palette reads the zero value as "search the file name".
func (t *tagIndex) lookup(path string) trackTags {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.entries[path]
}

// resolved reports whether a path has finished resolving, including a track
// that yielded no tags. A caller uses it to tell "not yet" from "nothing".
func (t *tagIndex) resolved(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.entries[path]

	return ok
}

// begin claims a path for resolution and reports whether the caller should
// start one. A path already cached or already in flight is not claimed.
func (t *tagIndex) begin(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.entries[path]; ok {
		return false
	}
	if _, ok := t.pending[path]; ok {
		return false
	}
	t.pending[path] = struct{}{}

	return true
}

// finish records a resolved path. An empty result is recorded too, so a file
// that yields no tags is not retried on every snapshot.
func (t *tagIndex) finish(path string, tags trackTags) {
	t.mu.Lock()
	t.entries[path] = tags
	delete(t.pending, path)
	t.mu.Unlock()
}

// keep drops everything not in the current queue, so a replaced queue cannot
// leave stale entries behind.
func (t *tagIndex) keep(current map[string]struct{}) {
	t.mu.Lock()
	for path := range t.entries {
		if _, ok := current[path]; !ok {
			delete(t.entries, path)
		}
	}
	t.mu.Unlock()
}

// indexQueue reconciles the tag index with the current queue: it drops entries
// for tracks that left and starts resolving the ones that have no tags yet.
func (s *PlayerService) indexQueue(paths []string) {
	current := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		current[p] = struct{}{}
	}
	s.index.keep(current)

	for _, p := range paths {
		if s.index.begin(p) {
			s.resolveTags(p)
		}
	}
}

// resolveTags reads one track's tags on a bounded worker. The meta chain is the
// same one the engine uses, so an untagged file falls through to its file name
// and the palette always has something to match.
func (s *PlayerService) resolveTags(path string) {
	go func() {
		select {
		case s.index.sem <- struct{}{}:
		case <-s.ctx.Done():
			s.index.finish(path, trackTags{})

			return
		}
		defer func() { <-s.index.sem }()

		m, err := meta.Resolve(s.ctx, path)
		if err != nil || m == nil {
			s.index.finish(path, trackTags{})

			return
		}
		s.index.finish(path, trackTags{
			Title:   m.Tags.Title,
			Artist:  m.Tags.Artist,
			Album:   m.Tags.Album,
			CoverID: cover.ID(m.Tags.Cover),
		})
	}()
}
