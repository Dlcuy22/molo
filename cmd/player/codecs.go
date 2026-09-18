package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/dlcuy22/player/decode"
)

// printCodecs writes the registry's codec list as plain lines. Recordan reads
// this over a pipe, so the listing must stay free of control bytes and of any
// column machinery that depends on a terminal width.
//
// The list is a snapshot of decode.Default rather than a constant: a new codec
// registers itself and appears here with no change to the CLI.
func printCodecs(w io.Writer) {
	codecs := decode.Default.Codecs()

	nameWidth := len("codec")
	for _, c := range codecs {
		nameWidth = max(nameWidth, len(c.Name))
	}

	fmt.Fprintf(w, "%-*s  %-12s  %6s  %s\n", nameWidth, "codec", "label", "weight", "auto")
	for _, c := range codecs {
		marker := ""
		if c.Default {
			marker = "(default)"
		}
		fmt.Fprintf(w, "%-*s  %-12s  %6d  %s\n", nameWidth, c.Name, codecLabel(c.FriendlyName), c.Weight, marker)
	}
}

// codecLabel keeps an unlabelled factory from shifting the weight column: a
// missing label is a real case for a factory without a Profile.
func codecLabel(friendly string) string {
	if friendly == "" {
		return "-"
	}

	return friendly
}

// codecList joins the codec names for an error message, where the table above
// would be unreadable.
func codecList() string {
	codecs := decode.Default.Codecs()
	parts := make([]string, len(codecs))
	for i, c := range codecs {
		parts[i] = c.Name
	}

	return strings.Join(parts, ", ")
}
