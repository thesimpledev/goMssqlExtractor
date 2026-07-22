package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// bracket returns the T-SQL quoted-identifier form of name.
func bracket(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

func qualified(schema, name string) string {
	return bracket(schema) + "." + bracket(name)
}

// safeName makes an object name usable as a file or directory name,
// including on Windows checkouts of the resulting git repository.
func safeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`/\:*?"<>|`, r) {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" || s == "." || s == ".." {
		s = "_"
	}
	return s
}

// writeSQLFile writes content with LF line endings and a trailing
// newline, creating parent directories as needed.
func writeSQLFile(path, content string) error {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// wipeOutput clears the parts of the output tree this run will
// regenerate, so objects dropped from the database disappear from the
// tree. A full run removes <out>/<database> entirely; a filtered run
// removes only the category directories being rewritten.
func wipeOutput(root string, inc map[string]bool, full bool) error {
	if full {
		return os.RemoveAll(root)
	}
	if inc["schemas"] {
		if err := os.RemoveAll(filepath.Join(root, "schemas")); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "schemas" {
			continue
		}
		for _, c := range categories {
			if c == "schemas" || !inc[c] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(root, e.Name(), c)); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeEmptyDirs prunes empty directories under root (root itself is
// removed too if the run produced nothing at all).
func removeEmptyDirs(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Reverse-lexical order visits children before their parents.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err == nil && len(entries) == 0 {
			_ = os.Remove(d)
		}
	}
	return nil
}
