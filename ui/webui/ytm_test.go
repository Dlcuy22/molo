package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/provider"
	ytm "github.com/dlcuy22/ytm-go"
)

// webmFixture is a real 2 s stereo WebM/Opus file, the same one the decode
// tests use. Serving it from an httptest server is what proves the provider
// hands the engine something the WebM reader accepts.
const webmFixture = "../../decode/testdata/webm_stereo_2s.webm"

func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(webmFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	return data
}

// mediaServer serves the fixture on every path and records whether the request
// carried the Range header the provider must send.
func mediaServer(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	data := fixtureBytes(t)
	var ranged atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			ranged.Store(true)
		}
		w.Header().Set("Content-Type", "audio/webm")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	return srv, &ranged
}

// ytmStreams builds a stream response pointing at url.
func ytmStreams(url string, expires time.Time) *ytm.Streams {
	return &ytm.Streams{
		VideoID:    "abc123",
		Title:      "Stream Title",
		Author:     "Stream Author",
		DurationMs: 2000,
		Formats: []ytm.StreamFormat{{
			Itag:      251,
			MimeType:  `audio/webm; codecs="opus"`,
			Codec:     ytm.CodecOpus,
			URL:       url,
			ExpiresAt: expires,
		}},
	}
}

func TestYTMRefRoundTrip(t *testing.T) {
	if got := ytmRef("MPEDabc123"); got != "ytm:abc123" {
		t.Fatalf("ytmRef = %q, want ytm:abc123", got)
	}
	if got := ytmID("ytm:abc123"); got != "abc123" {
		t.Fatalf("ytmID = %q, want abc123", got)
	}
	if !isYTMRef("ytm:abc") || isYTMRef("/m/a.opus") {
		t.Fatal("isYTMRef misclassifies a reference")
	}
}

func TestYTMProviderMatchAndName(t *testing.T) {
	p := newYTMProvider(newYTMIndex(), false, nil)
	if p.Name() != "ytm" {
		t.Fatalf("Name = %q", p.Name())
	}
	if !p.Match("ytm:x") || p.Match("/m/a.opus") {
		t.Fatal("Match misclassifies a reference")
	}
	if _, ok := interface{}(p).(provider.AudioProvider); !ok {
		t.Fatal("ytmProvider does not satisfy provider.AudioProvider")
	}
}

// TestYTMProviderOpenDecodes proves the whole path: a reference resolves to a
// source whose Opener produces a decoder that reads real audio.
func TestYTMProviderOpenDecodes(t *testing.T) {
	srv, ranged := mediaServer(t)

	index := newYTMIndex()
	index.put(ytmTrack{ID: "abc123", Title: "Catalogue Title", Artist: "Catalogue Artist", Album: "Catalogue Album"})
	p := &ytmProvider{
		index: index,
		http:  srv.Client(),
		resolve: func(context.Context, string) (*ytm.Streams, error) {
			return ytmStreams(srv.URL+"/media", time.Now().Add(time.Hour)), nil
		},
	}

	src, err := p.Open(context.Background(), "ytm:abc123")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if src.Local {
		t.Fatal("a remote source must not be marked Local")
	}
	if src.Opener == nil {
		t.Fatal("Open returned no Opener")
	}
	// The catalogue copy wins over the stream copy, because it carries the
	// album the stream response does not.
	if src.Meta.Tags.Title != "Catalogue Title" || src.Meta.Tags.Album != "Catalogue Album" {
		t.Fatalf("meta = %+v, want the catalogue title and album", src.Meta.Tags)
	}

	// The probe reports the catalogue duration in the engine's frame domain.
	info, err := src.Probe(core.DurationProbe)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if want := int64(2 * ytmSampleRate); info.TotalFrames != want {
		t.Fatalf("TotalFrames = %d, want %d", info.TotalFrames, want)
	}

	dec, err := src.Opener(make(chan struct{}))
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}
	defer dec.Close()

	if got := dec.Info().Format; got != core.CanonicalFormat {
		t.Fatalf("decoder format = %+v, want canonical", got)
	}
	buf := make([]float32, 4800)
	if n, err := dec.ReadFrames(buf); err != nil || n == 0 {
		t.Fatalf("ReadFrames = (%d, %v), want audio", n, err)
	}
	if !ranged.Load() {
		t.Fatal("the media request did not carry a Range header")
	}
}

// TestYTMProviderReResolvesExpiredURL pins the seek-correctness rule: a signed
// URL that has expired is replaced by a fresh one before the bytes are fetched,
// because the streamer calls the opener again on a seek.
func TestYTMProviderReResolvesExpiredURL(t *testing.T) {
	srv, _ := mediaServer(t)

	var calls atomic.Int32
	p := &ytmProvider{
		index: newYTMIndex(),
		http:  srv.Client(),
		resolve: func(context.Context, string) (*ytm.Streams, error) {
			if calls.Add(1) == 1 {
				// A dead URL: fetching it would fail the open.
				return ytmStreams(srv.URL+"/expired", time.Now().Add(-time.Hour)), nil
			}

			return ytmStreams(srv.URL+"/fresh", time.Now().Add(time.Hour)), nil
		},
	}

	src, err := p.Open(context.Background(), "ytm:abc123")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	dec, err := src.Opener(make(chan struct{}))
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}
	defer dec.Close()

	if got := calls.Load(); got != 2 {
		t.Fatalf("resolve calls = %d, want 2 (one at open, one on expiry)", got)
	}
}

// TestYTMQueueRowUsesCatalogueTitle pins the row behaviour that keeps the
// palette from showing a raw "ytm:..." reference as a title.
func TestYTMQueueRowUsesCatalogueTitle(t *testing.T) {
	index := newYTMIndex()
	row := ytmQueueRow(0, "ytm:abc123", 0, index)
	if row.Title != "" || row.Name != "" {
		t.Fatalf("unresolved row = %+v, want empty title and name", row)
	}
	if row.Path != "ytm:abc123" {
		t.Fatalf("row.Path = %q, want the reference", row.Path)
	}

	index.put(ytmTrack{ID: "abc123", Title: "Real Title", Artist: "Real Artist"})
	row = ytmQueueRow(0, "ytm:abc123", 0, index)
	if row.Title != "Real Title" || row.Name != "Real Title" {
		t.Fatalf("resolved row = %+v, want the catalogue title", row)
	}
}

func TestYTMResultsSongsFirstAndLabelled(t *testing.T) {
	album := &ytm.Playlist{ID: "MPREalbum", Name: "The Album", Type: ytm.PlaylistTypeAlbum}
	res := &ytm.SearchResults{
		Categories: []ytm.SearchCategory{
			{Layout: ytm.MediaItemLayout{Items: []ytm.MediaItem{
				&ytm.Artist{ID: "UCartist", Name: "An Artist"},
			}}},
			{Layout: ytm.MediaItemLayout{Items: []ytm.MediaItem{
				&ytm.Song{
					ID: "MPEDsong1", Name: "First Song",
					Artists: []ytm.Artist{{Name: "A"}, {Name: "B"}},
					Album:   album, DurationMs: 210000, IsExplicit: true,
					Type: ytm.SongTypeSong,
				},
				&ytm.Song{ID: "vid1", Name: "A Video", Type: ytm.SongTypeVideo},
				// A duplicate of the first song, as a second shelf can repeat.
				&ytm.Song{ID: "MPEDsong1", Name: "First Song", Type: ytm.SongTypeSong},
				// The same video id offered as a video by another shelf: one
				// identity, so it must not appear twice.
				&ytm.Song{ID: "MPEDsong1", Name: "First Song (Video)", Type: ytm.SongTypeVideo},
				album,
			}}},
		},
	}

	got := ytmResults(res)
	if len(got) != 4 {
		t.Fatalf("results = %d, want 4 (duplicate dropped)", len(got))
	}
	if got[0].Kind != "Song" || got[0].VideoID != "song1" || !got[0].Playable {
		t.Fatalf("first result = %+v, want the song first and playable", got[0])
	}
	if got[0].Artist != "A, B" {
		t.Fatalf("artist = %q, want %q", got[0].Artist, "A, B")
	}
	if got[0].Album != "The Album" || !got[0].Explicit || got[0].DurationMs != 210000 {
		t.Fatalf("song fields = %+v", got[0])
	}
	// A video is playable, but it sorts after every song.
	if got[1].Kind != "Video" || !got[1].Playable {
		t.Fatalf("second result = %+v, want the video", got[1])
	}
	// Non-playable kinds follow the playable ones, in catalogue order.
	if got[2].Kind != "Artist" || got[2].Playable {
		t.Fatalf("third result = %+v, want a non-playable artist", got[2])
	}
	if got[3].Kind != "Album" || got[3].Playable {
		t.Fatalf("fourth result = %+v, want a non-playable album", got[3])
	}
}

func TestYTMResultsEmpty(t *testing.T) {
	if got := ytmResults(nil); got != nil {
		t.Fatalf("ytmResults(nil) = %v, want nil", got)
	}
	if got := ytmResults(&ytm.SearchResults{}); len(got) != 0 {
		t.Fatalf("ytmResults(empty) = %v, want none", got)
	}
}

func TestYTMFramesUnknownIsNegative(t *testing.T) {
	if got := ytmFrames(0); got != -1 {
		t.Fatalf("ytmFrames(0) = %d, want -1", got)
	}
	if got := ytmFrames(1000); got != ytmSampleRate {
		t.Fatalf("ytmFrames(1000) = %d, want %d", got, ytmSampleRate)
	}
}

// TestSearchYTMSongsCachesAndIndexes drives the service's search through a fake
// client and asserts the two things the palette depends on: the result order
// and the catalogue entry a later queue row reads.
func TestSearchYTMSongsCachesAndIndexes(t *testing.T) {
	svc := newPlayerService()
	var calls atomic.Int32
	svc.ytmProv.search = func(context.Context, string, string, bool) (*ytm.SearchResults, error) {
		calls.Add(1)

		return &ytm.SearchResults{Categories: []ytm.SearchCategory{
			{Layout: ytm.MediaItemLayout{Items: []ytm.MediaItem{
				&ytm.Song{ID: "s1", Name: "A Song", Artists: []ytm.Artist{{Name: "X"}}},
			}}},
		}}, nil
	}

	first, err := svc.SearchYTMSongs("a song")
	if err != nil {
		t.Fatalf("SearchYTMSongs: %v", err)
	}
	if len(first) != 1 || first[0].Title != "A Song" || !first[0].Playable {
		t.Fatalf("results = %+v", first)
	}
	// The catalogue entry exists before the track is ever played, so a queue
	// row added now already has a title.
	if row := ytmQueueRow(0, "ytm:s1", 0, svc.ytm); row.Title != "A Song" {
		t.Fatalf("row = %+v, want the catalogue title", row)
	}

	if _, err := svc.SearchYTMSongs("a song"); err != nil {
		t.Fatalf("cached search: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("search calls = %d, want 1 (the second is cached)", got)
	}

	if got, err := svc.SearchYTMSongs("   "); err != nil || got != nil {
		t.Fatalf("blank search = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestPreviewRefusesRemoteRefs pins the structural rule: the preview player is
// built without providers, so a remote ref could never sound; refusing it keeps
// the main track from being left paused for a session that produces nothing.
func TestPreviewRefusesRemoteRefs(t *testing.T) {
	svc := newPlayerService()

	if err := svc.PreviewStart("ytm:abc123"); err == nil {
		t.Fatal("PreviewStart on a remote ref must fail")
	}
	if err := svc.PreviewStart(""); err == nil {
		t.Fatal("PreviewStart with no path must fail")
	}
}

// TestYTMQueueCommandsQueueTheReference proves the two palette commands hand
// the engine the provider reference and nothing else, and that "play next" is
// an insert rather than a queue replacement.
func TestYTMQueueCommandsQueueTheReference(t *testing.T) {
	// With nothing playing there is no "next", so Enter starts the track.
	fp := newFakePlayer()
	svc := newPlayerService()
	svc.player = fp

	if err := svc.InsertNextYTM("MPEDabc123"); err != nil {
		t.Fatalf("InsertNextYTM: %v", err)
	}
	if got := fp.callsSnapshot(); len(got) == 0 || got[len(got)-1] != "insertAndPlay:ytm:abc123" {
		t.Fatalf("calls = %v, want insertAndPlay:ytm:abc123", got)
	}

	// With a track playing, Enter inserts after it instead of cutting it off.
	fp = newFakePlayer()
	if err := fp.Play("/m/a.opus"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	svc = newPlayerService()
	svc.player = fp
	if err := svc.InsertNextYTM("abc123"); err != nil {
		t.Fatalf("InsertNextYTM: %v", err)
	}
	if got := fp.callsSnapshot(); len(got) == 0 || got[len(got)-1] != "insert:ytm:abc123" {
		t.Fatalf("calls = %v, want insert:ytm:abc123", got)
	}

	// Shift+Enter appends without starting.
	fp = newFakePlayer()
	svc = newPlayerService()
	svc.player = fp
	if err := svc.AppendYTM("abc123"); err != nil {
		t.Fatalf("AppendYTM: %v", err)
	}
	if got := fp.callsSnapshot(); len(got) == 0 || got[len(got)-1] != "insert:ytm:abc123" {
		t.Fatalf("calls = %v, want insert:ytm:abc123", got)
	}

	if err := svc.InsertNextYTM(""); err == nil {
		t.Fatal("InsertNextYTM with no id must fail")
	}
	if err := svc.AppendYTM(""); err == nil {
		t.Fatal("AppendYTM with no id must fail")
	}
}

// TestYTMPlaylistIDParsesURLAndBareID pins the input shapes the palette can hand
// over: the share URL a listener copies, a browse id, and a bare id.
func TestYTMPlaylistIDParsesURLAndBareID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://music.youtube.com/playlist?list=PLdirQjL5RyWE&si=DyvjQqw8h-cm07fx", "PLdirQjL5RyWE"},
		{"https://music.youtube.com/playlist?list=PLabc-123&si=x", "PLabc-123"},
		{"PLdirQjL5RyWE", "PLdirQjL5RyWE"},
		{"VLPLdirQjL5RyWE", "VLPLdirQjL5RyWE"},
		{"MPREb_album", "MPREb_album"},
		{"MPSPplaylist", "playlist"},
		{"  PLspace  ", "PLspace"},
		{"", ""},
	}
	for _, c := range cases {
		if got := ytmPlaylistID(c.in); got != c.want {
			t.Errorf("ytmPlaylistID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestYTMPlaylistResultsFlattensAndDedupes pins the row shape a playlist add
// caches: songs with credits, order preserved, a repeated id kept once.
func TestYTMPlaylistResultsFlattensAndDedupes(t *testing.T) {
	album := &ytm.Playlist{Name: "The Album"}
	pl := &ytm.Playlist{
		Name: "My Mix",
		Items: []ytm.Song{
			{ID: "MPEDone", Name: "One", Artists: []ytm.Artist{{Name: "A"}}, DurationMs: 1000, Album: album},
			{ID: "two", Name: "Two", Artists: []ytm.Artist{{Name: "B"}}},
			{ID: "MPEDone", Name: "One (repeat)", Artists: []ytm.Artist{{Name: "A"}}},
			{ID: "", Name: "No id"},
		},
	}

	got := ytmPlaylistResults(pl)
	if len(got) != 2 {
		t.Fatalf("results = %d, want 2 (duplicate and blank dropped)", len(got))
	}
	if got[0].VideoID != "one" || got[0].Title != "One" || !got[0].Playable {
		t.Fatalf("first = %+v, want the cleaned, playable song", got[0])
	}
	if got[0].Artist != "A" || got[0].Album != "The Album" {
		t.Fatalf("first credits = %+v", got[0])
	}
	if got[1].VideoID != "two" {
		t.Fatalf("second = %+v, want order preserved", got[1])
	}
}

// TestAddYTMPlaylistQueuesEveryTrack drives the service's playlist add through a
// fake catalogue and asserts both halves: the queue receives one reference per
// track in order, and the catalogue index is filled so a row has a title before
// anything plays.
func TestAddYTMPlaylistQueuesEveryTrack(t *testing.T) {
	fp := newFakePlayer()
	svc := newPlayerService()
	svc.player = fp
	svc.ytmProv.loadPlaylist = func(context.Context, string, *ytm.BuiltInContinuation) (*ytm.Playlist, error) {
		return &ytm.Playlist{Name: "Mix", Items: []ytm.Song{
			{ID: "s1", Name: "One"},
			{ID: "s2", Name: "Two"},
		}}, nil
	}

	if err := svc.AddYTMPlaylist("PLabc"); err != nil {
		t.Fatalf("AddYTMPlaylist: %v", err)
	}
	if got := fp.Queue(); len(got) != 2 || got[0] != "ytm:s1" || got[1] != "ytm:s2" {
		t.Fatalf("queue = %v, want [ytm:s1 ytm:s2]", got)
	}
	if row := ytmQueueRow(0, "ytm:s1", 0, svc.ytm); row.Title != "One" {
		t.Fatalf("row = %+v, want the catalogue title cached before play", row)
	}

	if err := svc.AddYTMPlaylist(""); err == nil {
		t.Fatal("AddYTMPlaylist with no id must fail")
	}
}

// TestAddYTMPlaylistFollowsContinuation proves a paged playlist is walked to the
// end, so a long list is not silently cut to its first page.
func TestAddYTMPlaylistFollowsContinuation(t *testing.T) {
	fp := newFakePlayer()
	svc := newPlayerService()
	svc.player = fp
	calls := 0
	svc.ytmProv.loadPlaylist = func(_ context.Context, _ string, cont *ytm.BuiltInContinuation) (*ytm.Playlist, error) {
		calls++
		if cont == nil {
			return &ytm.Playlist{
				Items:        []ytm.Song{{ID: "s1", Name: "One"}},
				Continuation: &ytm.BuiltInContinuation{Token: "next"},
			}, nil
		}

		return &ytm.Playlist{Items: []ytm.Song{{ID: "s2", Name: "Two"}}}, nil
	}

	if err := svc.AddYTMPlaylist("PLabc"); err != nil {
		t.Fatalf("AddYTMPlaylist: %v", err)
	}
	if calls != 2 {
		t.Fatalf("loadPlaylist calls = %d, want 2 (one page, one continuation)", calls)
	}
	if got := fp.Queue(); len(got) != 2 || got[1] != "ytm:s2" {
		t.Fatalf("queue = %v, want the continuation track included", got)
	}
}
