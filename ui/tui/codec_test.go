package tui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/decode"
)

// fakeCodecs is an injected chooser list. Its names are not in the process
// registry, which is what proves the picker renders the model's cached list
// rather than re-reading decode.Default every frame.
func fakeCodecs() []decode.Codec {
	return []decode.Codec{
		{Name: "alpha", FriendlyName: "Alpha", Weight: 90, Default: true},
		{Name: "beta", FriendlyName: "Beta", Weight: 80},
	}
}

// markedLine reports whether the line naming label carries the selection
// marker. It is how the tests read "which codec is marked" off the frame.
func markedLine(frame, label string) bool {
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, label) {
			return strings.HasPrefix(strings.TrimSpace(line), pickerMarker)
		}
	}

	return false
}

// openPicker builds a model with the injected list and opens the picker.
func openPicker(t *testing.T, f *fakePlayer) model {
	t.Helper()

	m := newModel(f)
	m.codecs = fakeCodecs()

	next, _ := m.Update(keyPress("c"))

	got, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", next)
	}
	if !got.pickerOpen {
		t.Fatal("c did not open the codec picker")
	}

	return got
}

// press drives one key and, when Update returns a Cmd, runs it and folds the
// resulting message back. That is the real event loop, and it is what carries
// an ApplySettings result to the model.
func press(t *testing.T, m model, key string) model {
	t.Helper()

	next, cmd := m.Update(keyPress(key))
	got := next.(model)
	if cmd == nil {
		return got
	}

	msg := cmd()
	next, _ = got.Update(msg)

	return next.(model)
}

// TestCodecPickerListsEveryCodec is the listing guarantee: every codec from the
// cached list, plus the explicit Auto row, appears with its friendly name and
// its registry name.
func TestCodecPickerListsEveryCodec(t *testing.T) {
	f := newFakePlayer()
	m := openPicker(t, f)

	frame := stripANSI(m.render())
	for _, want := range []string{"codec", "applies to the next track", "Auto (per file)", "Alpha (alpha)", "Beta (beta)"} {
		if !strings.Contains(frame, want) {
			t.Errorf("picker is missing %q:\n%s", want, frame)
		}
	}
}

// TestNewModelLoadsDefaultCodecs proves the cached list comes from the registry
// at construction, so the picker is populated in a real session.
func TestNewModelLoadsDefaultCodecs(t *testing.T) {
	m := newModel(newFakePlayer())

	for _, want := range []string{"opus-pion", "opus-libopusfile"} {
		found := false
		for _, c := range m.codecs {
			if c.Name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("default codecs missing %q: %+v", want, m.codecs)
		}
	}
}

// TestCodecPickerMarksAutoWhenSettingEmpty is the empty-string rule: with no
// explicit decoder the Auto row is the effective choice.
func TestCodecPickerMarksAutoWhenSettingEmpty(t *testing.T) {
	f := newFakePlayer()
	f.settings.Decoder = ""

	m := openPicker(t, f)
	if m.pickerCursor != 0 {
		t.Fatalf("cursor = %d, want 0 on the Auto row", m.pickerCursor)
	}

	frame := stripANSI(m.render())
	if !markedLine(frame, "Auto (per file)") {
		t.Fatalf("Auto row is not marked:\n%s", frame)
	}
	if markedLine(frame, "Alpha (alpha)") || markedLine(frame, "Beta (beta)") {
		t.Fatalf("a named codec is marked while the setting is auto:\n%s", frame)
	}
}

// TestCodecPickerMarksEffectiveCodec proves a named setting marks its row and
// nothing else.
func TestCodecPickerMarksEffectiveCodec(t *testing.T) {
	f := newFakePlayer()
	f.settings.Decoder = "beta"

	m := openPicker(t, f)
	if m.pickerCursor != 2 {
		t.Fatalf("cursor = %d, want 2 on beta", m.pickerCursor)
	}

	frame := stripANSI(m.render())
	if !markedLine(frame, "Beta (beta)") {
		t.Fatalf("beta row is not marked:\n%s", frame)
	}
	if markedLine(frame, "Auto (per file)") {
		t.Fatalf("Auto row is marked while beta is in effect:\n%s", frame)
	}
}

// TestCodecPickerSelectAppliesAndMarks is the success path: confirming a codec
// calls ApplySettings with that name and the marker lands on it.
func TestCodecPickerSelectAppliesAndMarks(t *testing.T) {
	f := newFakePlayer()
	m := openPicker(t, f)

	// Auto -> alpha -> beta.
	for _, k := range []string{"down", "down"} {
		m = press(t, m, k)
	}

	m = press(t, m, "enter")

	if !f.called("ApplySettings") {
		t.Fatalf("enter did not call ApplySettings: %v", f.calls)
	}
	if got := f.Settings().Decoder; got != "beta" {
		t.Fatalf("engine decoder = %q, want beta", got)
	}
	if m.codecSetting != "beta" {
		t.Fatalf("model codec setting = %q, want beta", m.codecSetting)
	}

	frame := stripANSI(m.render())
	if !markedLine(frame, "Beta (beta)") {
		t.Fatalf("marker did not move to beta:\n%s", frame)
	}
}

// TestCodecPickerSelectAutoAppliesEmpty is the other half of the empty rule:
// choosing Auto calls ApplySettings with "".
func TestCodecPickerSelectAutoAppliesEmpty(t *testing.T) {
	f := newFakePlayer()
	f.settings.Decoder = "beta"

	m := openPicker(t, f)

	// beta -> alpha -> Auto.
	for _, k := range []string{"up", "up"} {
		m = press(t, m, k)
	}

	m = press(t, m, "enter")

	if got := f.Settings().Decoder; got != "" {
		t.Fatalf("engine decoder = %q, want the empty automatic preference", got)
	}
	if m.codecSetting != "" {
		t.Fatalf("model codec setting = %q, want empty", m.codecSetting)
	}

	frame := stripANSI(m.render())
	if !markedLine(frame, "Auto (per file)") {
		t.Fatalf("marker did not move to Auto:\n%s", frame)
	}
}

// TestCodecPickerRejectionKeepsSelection is the failure path: a rejected
// ApplySettings is surfaced and the marked selection does not move.
func TestCodecPickerRejectionKeepsSelection(t *testing.T) {
	f := newFakePlayer()
	f.settings.Decoder = "alpha"
	f.applyErr = player.ErrInvalidSetting

	m := openPicker(t, f)
	if m.pickerCursor != 1 {
		t.Fatalf("precondition: cursor = %d, want 1 on alpha", m.pickerCursor)
	}

	m = press(t, m, "down")
	m = press(t, m, "enter")

	if m.pickerErr == nil {
		t.Fatal("rejection was silently ignored")
	}
	if !errors.Is(m.pickerErr, player.ErrInvalidSetting) {
		t.Fatalf("picker error = %v, want ErrInvalidSetting", m.pickerErr)
	}
	if m.codecSetting != "alpha" {
		t.Fatalf("rejected selection changed the setting to %q", m.codecSetting)
	}

	frame := stripANSI(m.render())
	if !strings.Contains(frame, "error:") {
		t.Fatalf("picker does not show the error:\n%s", frame)
	}
	if !markedLine(frame, "Alpha (alpha)") {
		t.Fatalf("previous selection is no longer marked:\n%s", frame)
	}
	if markedLine(frame, "Beta (beta)") {
		t.Fatalf("rejected selection is marked:\n%s", frame)
	}
}

// TestCodecPickerKeyboardLifecycle covers open, move, cancel and toggle.
func TestCodecPickerKeyboardLifecycle(t *testing.T) {
	f := newFakePlayer()
	m := openPicker(t, f)

	next, _ := m.Update(keyPress("down"))
	m = next.(model)
	if m.pickerCursor != 1 {
		t.Fatalf("down moved to %d, want 1", m.pickerCursor)
	}

	next, _ = m.Update(keyPress("up"))
	m = next.(model)
	if m.pickerCursor != 0 {
		t.Fatalf("up moved to %d, want 0", m.pickerCursor)
	}

	next, _ = m.Update(keyPress("esc"))
	m = next.(model)
	if m.pickerOpen {
		t.Fatal("esc did not close the picker")
	}

	next, _ = m.Update(keyPress("c"))
	m = next.(model)
	if !m.pickerOpen {
		t.Fatal("picker did not reopen after esc")
	}

	next, _ = m.Update(keyPress("c"))
	m = next.(model)
	if m.pickerOpen {
		t.Fatal("c did not toggle the picker closed")
	}
}

// TestCodecPickerUnknownKeysDoNotLeak is the isolation guarantee: while the
// picker is open, a key it does not use must not reach the engine and must not
// close the picker.
func TestCodecPickerUnknownKeysDoNotLeak(t *testing.T) {
	f := newFakePlayer()
	f.snap.State = player.Playing
	m := openPicker(t, f)

	before := f.callCount()
	for _, k := range []string{"n", "p", "space", "l", "h", "+", "-", "x", "tab"} {
		next, cmd := m.Update(keyPress(k))
		m = next.(model)
		if cmd != nil {
			t.Fatalf("%q returned a command inside the picker", k)
		}
	}

	if f.callCount() != before {
		t.Fatalf("keys leaked to the engine while the picker was open: %v", f.calls)
	}
	if !m.pickerOpen {
		t.Fatal("a stray key closed the picker")
	}
}

// TestCodecPickerQuitStillWorks proves quit is not trapped by the modal: it is
// a program-level action, so a user is never stranded in the picker.
func TestCodecPickerQuitStillWorks(t *testing.T) {
	m := openPicker(t, newFakePlayer())

	for _, k := range []string{"q", "ctrl+c"} {
		next, cmd := m.Update(keyPress(k))
		m = next.(model)
		if cmd == nil {
			t.Fatalf("%q inside the picker returned no quit command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%q inside the picker produced %T, want tea.QuitMsg", k, cmd())
		}
	}
}

// TestMainControlsWorkWhenPickerClosed is the counterweight: with the picker
// closed, the existing bindings still reach the engine.
func TestMainControlsWorkWhenPickerClosed(t *testing.T) {
	f := newFakePlayer()
	f.snap.State = player.Playing
	m := newModel(f)

	next, _ := m.Update(keyPress("n"))
	m = next.(model)
	if !f.called("Next") {
		t.Fatalf("n did not reach the engine: %v", f.calls)
	}

	next, _ = m.Update(keyPress("space"))
	m = next.(model)
	if !f.called("Pause") {
		t.Fatalf("space did not reach the engine: %v", f.calls)
	}
	if m.pickerOpen {
		t.Fatal("a main control opened the picker")
	}
}

// TestCodecPickerLogsSelection proves the debug panel records the change: a
// success logs the codec, a rejection logs the error.
func TestCodecPickerLogsSelection(t *testing.T) {
	f := newFakePlayer()
	m := openPicker(t, f)

	m = press(t, m, "down")
	m = press(t, m, "enter")

	joined := strings.Join(m.debug.lines, "\n")
	if !strings.Contains(joined, "codec    alpha") {
		t.Fatalf("debug log missing the accepted codec:\n%s", joined)
	}

	f.applyErr = player.ErrInvalidSetting
	m = press(t, m, "down")
	m = press(t, m, "enter")

	joined = strings.Join(m.debug.lines, "\n")
	if !strings.Contains(joined, "error    ") {
		t.Fatalf("debug log missing the rejected codec error:\n%s", joined)
	}
}

// TestScriptedCodecPickerRender drives the real Bubble Tea program against a
// fake engine, opens the picker, walks to the other codec and confirms it. It
// prints the captured frames so the picker is visible as evidence, not just
// asserted.
func TestScriptedCodecPickerRender(t *testing.T) {
	f := newFakePlayer()

	m := newModel(f)
	m.width, m.height = 64, 18
	m.codecs = fakeCodecs()

	// c opens, j j walks Auto -> alpha -> beta, enter selects, c closes, q quits.
	in := &pacedReader{data: []byte("cjj\rcq"), delay: 60 * time.Millisecond}
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p := tea.NewProgram(
		m,
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(&out),
		tea.WithWindowSize(64, 18),
	)
	if _, err := p.Run(); err != nil {
		t.Fatalf("program: %v", err)
	}

	frames := stripANSI(out.String())
	t.Logf("captured codec picker frames:\n%s", frames)

	if !strings.Contains(frames, "Auto (per file)") || !strings.Contains(frames, "Beta (beta)") {
		t.Errorf("captured frames do not show the picker list:\n%s", frames)
	}
	if got := f.Settings().Decoder; got != "beta" {
		t.Errorf("scripted selection landed on %q, want beta", got)
	}
}
