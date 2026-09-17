package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// resolveQueue turns command-line arguments into the ordered list of files to
// play. A file argument is taken as given, because the decoder registry can
// identify a file by content even when its extension is unfamiliar. A
// directory argument expands to every decodable file underneath it, recursively,
// in sorted order so two runs over the same tree produce the same queue.
//
// Duplicates are dropped, keeping the first occurrence: listing a file and the
// directory that contains it is a normal thing to type and must not play it
// twice.
func resolveQueue(args []string, supported []string) ([]string, error) {
	exts := make(map[string]struct{}, len(supported))
	for _, ext := range supported {
		exts[strings.ToLower(ext)] = struct{}{}
	}

	var queue []string
	seen := make(map[string]struct{})

	add := func(path string) {
		key := filepath.Clean(path)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		queue = append(queue, path)
	}

	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, fmt.Errorf("cannot read %s: %w", arg, err)
		}

		if !info.IsDir() {
			add(arg)

			continue
		}

		found, err := scanDir(arg, exts)
		if err != nil {
			return nil, err
		}
		for _, path := range found {
			add(path)
		}
	}

	return queue, nil
}

// scanDir lists the decodable files under root. WalkDir is used rather than a
// manual recursion because it does not follow directory symlinks, so a link
// cycle cannot turn the scan into an infinite loop.
func scanDir(root string, exts map[string]struct{}) ([]string, error) {
	var found []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		if _, ok := exts[strings.ToLower(filepath.Ext(path))]; ok {
			found = append(found, path)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cannot scan %s: %w", root, err)
	}

	return found, nil
}
