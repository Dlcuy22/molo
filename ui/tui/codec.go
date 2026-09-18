package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/dlcuy22/player"
)

// The codec picker is a modal list over the now-playing panel. It exists
// because a decoder preference is read when the next track is built, so the UI
// must let the user choose without pretending the sound already changed.
const (
	// pickerMarker flags the codec currently in effect, exactly as the queue
	// marker flags the current track.
	pickerMarker = activeMarker
	// cursorMarker flags where the keyboard cursor is. It is a separate cue
	// from the effective marker, so browsing never looks like a commitment.
	cursorMarker = ">"
	// pickerAutoLabel is the explicit automatic entry. It is offered alongside
	// the named codecs rather than hidden, because "" is a real choice the
	// engine understands (per-file selection by weight).
	pickerAutoLabel = "Auto (per file)"
)

// openPicker shows the picker with the cursor on the effective codec. The list
// is the model's cached copy, so opening costs no registry read.
func (m model) openPicker() model {
	m.pickerOpen = true
	m.pickerErr = nil
	m.pickerCursor = m.codecIndex(m.codecSetting)

	return m
}

// codecIndex returns the list row for a decoder name, defaulting to the Auto
// row. A name the engine accepts but this build no longer lists still lands on
// Auto rather than out of range.
func (m model) codecIndex(name string) int {
	if name == "" {
		return 0
	}
	for i, c := range m.codecs {
		if c.Name == name {
			return i + 1
		}
	}

	return 0
}

// codecRows is the number of picker entries: one Auto row plus one per codec.
func (m model) codecRows() int { return len(m.codecs) + 1 }

// codecNameAt maps a row back to the decoder name ApplySettings expects. The
// Auto row is the empty string, the engine's spelling of "automatic".
func (m model) codecNameAt(row int) string {
	if row <= 0 || row > len(m.codecs) {
		return ""
	}

	return m.codecs[row-1].Name
}

// codecLabel renders one row: the friendly name from the registry profile, with
// the registry name in parentheses so a user can match it to configuration and
// debug output.
func (m model) codecLabel(name string) string {
	if name == "" {
		return pickerAutoLabel
	}
	for _, c := range m.codecs {
		if c.Name != name {
			continue
		}
		if c.FriendlyName != "" {
			return fmt.Sprintf("%s (%s)", c.FriendlyName, c.Name)
		}

		return c.Name
	}

	return name
}

// movePickerCursor walks the list, clamped at both ends. A short list must not
// wrap: wrapping would make the Auto row reachable by accident from the last
// entry, which is a poor default for a three-item choice.
func (m model) movePickerCursor(delta int) model {
	row := m.pickerCursor + delta
	if row < 0 {
		row = 0
	}
	if row >= m.codecRows() {
		row = m.codecRows() - 1
	}
	m.pickerCursor = row

	return m
}

// handlePickerKey is the modal key map. Anything it does not name is swallowed,
// so a stray press cannot reach the main view's controls while the picker is
// open. Quit stays available: it is a program-level action, not a main-view
// control, and trapping it would strand the user in the modal.
func (m model) handlePickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		return m.movePickerCursor(-1), nil

	case "down", "j":
		return m.movePickerCursor(1), nil

	case "enter":
		name := m.codecNameAt(m.pickerCursor)
		if m.p == nil {
			m.pickerErr = nil
			m.codecSetting = name

			return m, nil
		}
		// ApplySettings validates locally and never touches disk or the
		// device, so it runs as a Cmd only for the same reason every engine
		// call does: Update must not assume any engine call is instant.
		return m, applyCodec(m.p, name)

	case "esc", "c":
		m.pickerOpen = false
		m.pickerErr = nil

		return m, nil

	case "q", "ctrl+c":
		m.stopWave()

		return m, tea.Quit
	}

	return m, nil
}

// applyCodec reads the current settings and applies the chosen decoder, leaving
// the other fields untouched. It runs on the Cmd goroutine; the result comes
// back as codecResultMsg so the marker only moves once the engine has accepted.
func applyCodec(p player.Player, name string) tea.Cmd {
	return func() tea.Msg {
		next := p.Settings()
		next.Decoder = name
		if err := p.ApplySettings(next); err != nil {
			return codecResultMsg{name: name, err: err}
		}

		return codecResultMsg{name: name}
	}
}

// applyCodecResult folds a validated selection back into the model. A rejected
// update leaves the effective setting alone and surfaces the error; an accepted
// one moves the marker and clears any previous rejection.
func (m model) applyCodecResult(msg codecResultMsg) model {
	if msg.err != nil {
		m.pickerErr = msg.err
		m.debug.push(formatErrorLine(msg.err))

		return m
	}

	m.codecSetting = msg.name
	m.pickerErr = nil
	m.debug.push(formatCodecLine(msg.name))

	return m
}

// renderPicker draws the modal list. The marker shows the effective codec and
// the cursor shows where a confirmation would land; the header states the one
// timing fact that matters, that the change applies to the next track.
func (m model) renderPicker(width int) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("codec"))
	b.WriteString("  ")
	b.WriteString(dimStyle.Render("applies to the next track"))
	b.WriteString("\n")

	for row := 0; row < m.codecRows(); row++ {
		prefix := "  "
		if m.codecNameAt(row) == m.codecSetting {
			prefix = pickerMarker + " "
		} else if row == m.pickerCursor {
			prefix = cursorMarker + " "
		}

		line := clip(prefix+m.codecLabel(m.codecNameAt(row)), width)
		if m.codecNameAt(row) == m.codecSetting || row == m.pickerCursor {
			b.WriteString(accentStyle.Render(line))
		} else {
			b.WriteString(dimStyle.Render(line))
		}
		b.WriteString("\n")
	}

	if m.pickerErr != nil {
		b.WriteString(errStyle.Render(clip("error: "+m.pickerErr.Error(), width)))
		b.WriteString("\n")
	}

	b.WriteString(dimStyle.Render("enter apply  esc cancel"))

	return b.String()
}
