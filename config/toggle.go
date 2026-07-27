package config

import (
	"fmt"
	"regexp"
	"strings"
)

// SetPaused returns a copy of a raw TOML config with the pause flag for a
// dataset (or globally, when dataset is "") set or cleared. It edits the
// text line-by-line rather than re-serializing, so comments and formatting
// survive; clearing a pause removes the line entirely rather than writing
// `paused = false`. The result is re-parsed to verify both that it is
// valid TOML and that the toggle had the intended effect.
func SetPaused(raw []byte, dataset string, paused bool) ([]byte, error) {
	var out []byte
	var err error
	if dataset == "" {
		out, err = setGlobalPaused(raw, paused)
	} else {
		out, err = setDatasetPaused(raw, dataset, paused)
	}
	if err != nil {
		return nil, err
	}

	conf, err := Parse(out)
	if err != nil {
		return nil, fmt.Errorf("edit produced invalid config: %w", err)
	}
	want := paused
	var got bool
	if dataset == "" {
		got = conf.Paused
	} else {
		got = conf.PausedFor(dataset)
	}
	if got != want {
		return nil, fmt.Errorf("edit did not take effect: paused(%q) = %v, want %v", dataset, got, want)
	}
	return out, nil
}

var (
	pausedLineRe  = regexp.MustCompile(`^\s*paused\s*=`)
	sectionLineRe = regexp.MustCompile(`^\s*\[`)
)

func setGlobalPaused(raw []byte, paused bool) ([]byte, error) {
	lines := strings.Split(string(raw), "\n")

	// The top-level region ends at the first section header.
	end := len(lines)
	for i, line := range lines {
		if sectionLineRe.MatchString(line) {
			end = i
			break
		}
	}

	for i := 0; i < end; i++ {
		if pausedLineRe.MatchString(lines[i]) {
			if paused {
				lines[i] = "paused = true"
			} else {
				lines = append(lines[:i], lines[i+1:]...)
			}
			return []byte(strings.Join(lines, "\n")), nil
		}
	}

	if !paused {
		return raw, nil
	}

	// Insert after the last non-blank top-level line, so the flag sits
	// with snitch_id and friends rather than floating before a section.
	at := 0
	for i := 0; i < end; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			at = i + 1
		}
	}
	lines = append(lines[:at], append([]string{"paused = true"}, lines[at:]...)...)
	return []byte(strings.Join(lines, "\n")), nil
}

func setDatasetPaused(raw []byte, dataset string, paused bool) ([]byte, error) {
	dataset = normalizeOverrideKey(dataset)
	lines := strings.Split(string(raw), "\n")

	// Find the override's base section header, `[overrides."<key>"]`
	// (subsection headers like `[overrides."<key>".local.policy]` don't
	// count). The section body runs to the next header of any kind.
	header := -1
	for i, line := range lines {
		key, ok := overrideSectionKey(line)
		if ok && normalizeOverrideKey(key) == dataset {
			header = i
			break
		}
	}

	if header == -1 {
		if !paused {
			return raw, nil
		}
		src := strings.TrimRight(string(raw), "\n")
		return fmt.Appendf(nil, "%s\n\n[overrides.%q]\npaused = true\n", src, dataset), nil
	}

	end := len(lines)
	for i := header + 1; i < len(lines); i++ {
		if sectionLineRe.MatchString(lines[i]) {
			end = i
			break
		}
	}

	for i := header + 1; i < end; i++ {
		if pausedLineRe.MatchString(lines[i]) {
			if paused {
				lines[i] = "paused = true"
			} else {
				lines = append(lines[:i], lines[i+1:]...)
			}
			return []byte(strings.Join(lines, "\n")), nil
		}
	}

	if !paused {
		return raw, nil
	}
	lines = append(lines[:header+1], append([]string{"paused = true"}, lines[header+1:]...)...)
	return []byte(strings.Join(lines, "\n")), nil
}

// overrideSectionKey extracts the override key from a base override
// section header line: `[overrides."/tm"]` yields "/tm". Lines that are
// not override headers, or that address a subsection (a dotted path after
// the key, like `[overrides."/tm".local.policy]`), return ok=false.
func overrideSectionKey(line string) (key string, ok bool) {
	s := strings.TrimSpace(line)
	if found := strings.Contains(s, "#"); found && !strings.HasPrefix(s, "[") {
		return "", false
	}
	const prefix = "[overrides."
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	rest, found := strings.CutSuffix(afterComment(s[len(prefix):]), "]")
	if !found {
		return "", false
	}

	if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'') {
		quote := rest[0]
		close := strings.IndexByte(rest[1:], quote)
		if close < 0 {
			return "", false
		}
		key = rest[1 : 1+close]
		if rest[1+close+1:] != "" {
			return "", false // subsection: trailing dotted path
		}
		return key, true
	}

	if strings.Contains(rest, ".") {
		return "", false // subsection of a bare key
	}
	return rest, true
}

// afterComment strips a trailing comment and surrounding whitespace from
// the remainder of a section header line.
func afterComment(s string) string {
	// A '#' inside quotes would be part of the key; keys are dataset
	// paths, which never contain '#', so a simple cut is fine.
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
