package history

import (
	"bufio"
	"io"
	"regexp"
	"strings"
	"time"
)

// Outcome classifies one cache restore.
type Outcome string

const (
	// OutcomeHit: the entry restored under the exact primary key.
	OutcomeHit Outcome = "hit"
	// OutcomePartial: a restore-keys prefix matched an older entry, so the
	// job started warm but will save a new entry.
	OutcomePartial Outcome = "partial"
	// OutcomeMiss: nothing matched; the job started cold.
	OutcomeMiss Outcome = "miss"
)

// CacheEvent is one cache restore found in a job log, plus the save that
// followed it in the post step, if any.
type CacheEvent struct {
	// Action is the uses: reference of the step, e.g. "actions/cache@v6".
	Action     string  `json:"action"`
	PrimaryKey string  `json:"primary_key,omitempty"`
	MatchedKey string  `json:"matched_key,omitempty"`
	Outcome    Outcome `json:"outcome"`
	// RestoreSeconds runs from the step's start to the restore outcome line.
	RestoreSeconds float64 `json:"restore_seconds"`
	// Saved is true when the post step uploaded a new entry; SaveSeconds is
	// then the time from the post step's start to the "saved" line.
	Saved       bool    `json:"saved"`
	SaveSeconds float64 `json:"save_seconds,omitempty"`

	hasSaveLine bool
}

// The patterns below are the info lines actions/cache (restore and save) and
// the setup-* actions' built-in caches write, taken from their sources:
// actions/cache src/restoreImpl.ts and src/saveImpl.ts, and the cache-restore /
// cache-save modules of setup-node, setup-go, setup-python and setup-java.
var (
	restoredRe  = regexp.MustCompile(`(?i)cache restored from key: (.+)$`)
	lookupRe    = regexp.MustCompile(`^Cache found and can be restored from key: (.+)$`)
	notFoundRe  = regexp.MustCompile(`^Cache not found for input keys: (.+)$`)
	setupMissRe = regexp.MustCompile(`(?i)cache is not found$`)
	savedRe     = regexp.MustCompile(`^Cache saved with (the )?key: (.+)$`)
	skipSaveRe  = regexp.MustCompile(`^Cache hit occurred on the primary key (.+), not saving cache\.$`)
	withKeyRe   = regexp.MustCompile(`^\s+key: (.+)$`)
	usesRefRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(/[A-Za-z0-9_./-]+)?@\S+$`)
	ansiRe      = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")
)

// ParseCacheLog extracts cache events from a raw job log. Every log line
// starts with an RFC 3339 timestamp; "##[group]Run <x>" opens a step and
// "Post job cleanup." opens a post step, which is where saves happen.
func ParseCacheLog(r io.Reader) ([]CacheEvent, error) {
	var (
		events    []*CacheEvent
		stepStart time.Time
		action    string // uses: ref of the current step, "" for run: steps
		withKey   string // key: input echoed in the step header
		inHeader  bool
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		ts, text, ok := splitLogLine(sc.Text())
		if !ok {
			continue
		}
		if rest, ok := strings.CutPrefix(text, "##[group]Run "); ok {
			stepStart = ts
			action, withKey, inHeader = "", "", true
			if ref := strings.TrimSpace(rest); usesRefRe.MatchString(ref) {
				action = ref
			}
			continue
		}
		if text == "Post job cleanup." {
			stepStart = ts
			action, withKey, inHeader = "", "", false
			continue
		}
		if inHeader {
			if text == "##[endgroup]" {
				inHeader = false
			} else if m := withKeyRe.FindStringSubmatch(text); m != nil && withKey == "" {
				withKey = strings.TrimSpace(m[1])
			}
			continue
		}

		secs := ts.Sub(stepStart).Seconds()
		switch {
		case lookupRe.MatchString(text):
			m := lookupRe.FindStringSubmatch(text)
			events = append(events, &CacheEvent{Action: action, PrimaryKey: withKey, MatchedKey: m[1], RestoreSeconds: secs})
		case restoredRe.MatchString(text):
			m := restoredRe.FindStringSubmatch(text)
			events = append(events, &CacheEvent{Action: action, PrimaryKey: withKey, MatchedKey: m[1], RestoreSeconds: secs})
		case notFoundRe.MatchString(text):
			m := notFoundRe.FindStringSubmatch(text)
			primary := withKey
			if primary == "" {
				primary, _, _ = strings.Cut(m[1], ", ")
			}
			events = append(events, &CacheEvent{Action: action, PrimaryKey: primary, Outcome: OutcomeMiss, RestoreSeconds: secs})
		case setupMissRe.MatchString(text):
			events = append(events, &CacheEvent{Action: action, Outcome: OutcomeMiss, RestoreSeconds: secs})
		case savedRe.MatchString(text):
			m := savedRe.FindStringSubmatch(text)
			if e := matchSave(events, m[2], m[1] != ""); e != nil {
				e.hasSaveLine, e.Saved, e.SaveSeconds = true, true, secs
			}
		case skipSaveRe.MatchString(text):
			m := skipSaveRe.FindStringSubmatch(text)
			if e := matchSave(events, m[1], true); e != nil {
				e.hasSaveLine = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	out := make([]CacheEvent, 0, len(events))
	for _, e := range events {
		if e.Outcome != OutcomeMiss {
			// actions/cache compares keys case-insensitively (isExactKeyMatch).
			if e.PrimaryKey == "" || strings.EqualFold(e.PrimaryKey, e.MatchedKey) {
				e.Outcome = OutcomeHit
			} else {
				e.Outcome = OutcomePartial
			}
		}
		out = append(out, *e)
	}
	return out, nil
}

// matchSave finds the restore a save line belongs to: the one with the same
// primary key, else the same matched key, else (for setup-* saves, which
// never echo a key in the restore step) the first setup-* restore with no
// key yet. The setup-* key is filled in so its prefix can be grouped.
func matchSave(events []*CacheEvent, key string, setupStyle bool) *CacheEvent {
	for _, e := range events {
		if !e.hasSaveLine && e.PrimaryKey != "" && strings.EqualFold(e.PrimaryKey, key) {
			return e
		}
	}
	for _, e := range events {
		if !e.hasSaveLine && e.MatchedKey != "" && strings.EqualFold(e.MatchedKey, key) {
			if e.PrimaryKey == "" {
				e.PrimaryKey = key
			}
			return e
		}
	}
	if setupStyle {
		for _, e := range events {
			if !e.hasSaveLine && e.PrimaryKey == "" && !isCacheAction(e.Action) {
				e.PrimaryKey = key
				return e
			}
		}
	}
	return nil
}

// isCacheAction reports whether ref is actions/cache or one of its
// restore/save sub-actions.
func isCacheAction(ref string) bool {
	name, _, _ := strings.Cut(ref, "@")
	return strings.EqualFold(name, "actions/cache") || strings.HasPrefix(strings.ToLower(name), "actions/cache/")
}

// splitLogLine separates the leading timestamp from a log line.
func splitLogLine(line string) (time.Time, string, bool) {
	line = strings.TrimPrefix(line, "\ufeff")
	stamp, text, ok := strings.Cut(line, " ")
	if !ok {
		return time.Time{}, "", false
	}
	ts, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, "", false
	}
	text = ansiRe.ReplaceAllString(strings.TrimRight(text, "\r"), "")
	return ts, text, true
}

// hashRunRe finds the hashFiles() output (or any long hex digest) in a key.
var hashRunRe = regexp.MustCompile(`(?i)[0-9a-f]{16,}`)

// KeyPrefix returns a key with its first hash and everything after it cut,
// e.g. "Linux-node-3f2a…" -> "Linux-node-". Keys without a hash stay whole.
// When no key is known (a setup-* miss with no save) the action name is used.
func (e CacheEvent) KeyPrefix() string {
	key := e.PrimaryKey
	if key == "" {
		key = e.MatchedKey
	}
	if key == "" {
		name, _, _ := strings.Cut(e.Action, "@")
		if name == "" {
			name = "unknown"
		}
		return "(" + name + ")"
	}
	if loc := hashRunRe.FindStringIndex(key); loc != nil {
		return key[:loc[0]]
	}
	return key
}
