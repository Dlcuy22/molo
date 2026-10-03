package main

import (
	"strings"

	"github.com/dlcuy22/molo"
)

// codecLabel builds a row label as "{Family} {FriendlyName}". The family is the
// part of the registry name before its first dash, so verbose variants of one
// codec collapse to a recognizable prefix: opus-pion and opus-pion-exact both
// read "Opus ...". A name already equal to its family is not repeated, so
// "flac" reads "Flac Lossless" rather than "Flac Flac".
func codecLabel(c molo.Codec) string {
	reg := c.Name
	friendly := c.FriendlyName
	if friendly == "" {
		friendly = reg
	}
	family := reg
	if i := strings.IndexByte(reg, '-'); i > 0 {
		family = reg[:i]
	}
	family = titleFirst(family)
	friendly = titleFirst(friendly)
	if strings.EqualFold(family, friendly) {
		return friendly
	}

	return family + " " + friendly
}

// titleFirst upper-cases the first byte of an ASCII word. Registry names and
// friendly labels are ASCII, so this needs no unicode machinery.
func titleFirst(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-'a'+'A') + s[1:]
	}

	return s
}
