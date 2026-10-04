package proxy

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// redactor is the privacy firewall: it finds secrets/PII in an outbound request
// and replaces them with reversible placeholders, so they never reach a
// third-party LLM. A restoringWriter puts the originals back in the response, so
// the masking is invisible to the client (and to Claude Code's file edits).
type redactor struct{}

type detector struct {
	name  string
	re    *regexp.Regexp
	group int // submatch index to redact (0 = whole match)
}

// Detectors are ordered most-specific first. They target high-confidence secret
// shapes + email, to avoid mangling ordinary code.
var detectors = []detector{
	{"privatekey", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), 0},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{6,}`), 0},
	{"apikey", regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}|sk-[A-Za-z0-9]{20,}|gsk_[A-Za-z0-9]{20,}|AIza[A-Za-z0-9_\-]{20,}|ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{40,}|xox[baprs]-[A-Za-z0-9\-]{10,}|AKIA[0-9A-Z]{16}`), 0},
	{"secret", regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|password|passwd|access[_-]?key|client[_-]?secret)\b["']?\s*[:=]\s*["']?([A-Za-z0-9_\-.+/]{8,})`), 1},
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), 0},
}

const phPrefix = "[NX_REDACTED_"

// redact walks every string value in the JSON request body and replaces detected
// secrets with placeholders. It returns the redacted body and a placeholder →
// original map (empty/nil if nothing was found).
func (r *redactor) redact(body []byte) ([]byte, map[string]string) {
	var v interface{}
	if json.Unmarshal(body, &v) != nil {
		return body, nil
	}
	repl := map[string]string{}
	counter := map[string]int{}
	var walk func(x interface{}) interface{}
	walk = func(x interface{}) interface{} {
		switch t := x.(type) {
		case string:
			if isBase64DataURI(t) {
				return t // OpenAI image_url payload: never rewrite image bytes
			}
			return redactString(t, repl, counter)
		case []interface{}:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		case map[string]interface{}:
			// Anthropic image source {"type":"base64","data":"..."}: the data
			// value is image bytes, not text; a detector match inside it would
			// corrupt the image.
			skipData := false
			if typ, ok := t["type"].(string); ok && typ == "base64" {
				skipData = true
			}
			for k := range t {
				if skipData && k == "data" {
					continue
				}
				t[k] = walk(t[k])
			}
			return t
		}
		return x
	}
	v = walk(v)
	if len(repl) == 0 {
		return body, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return body, nil
	}
	return out, repl
}

// isBase64DataURI reports whether s is a base64 data URI ("data:<mime>;base64,...").
// It is a cheap prefix test (no regex): the ";base64," marker must appear within
// the first 128 bytes.
func isBase64DataURI(s string) bool {
	if !strings.HasPrefix(s, "data:") {
		return false
	}
	head := s
	if len(head) > 128 {
		head = head[:128]
	}
	return strings.Contains(head, ";base64,")
}

func redactString(s string, repl map[string]string, counter map[string]int) string {
	for _, d := range detectors {
		locs := d.re.FindAllStringSubmatchIndex(s, -1)
		if locs == nil {
			continue
		}
		for i := len(locs) - 1; i >= 0; i-- { // right-to-left keeps indices valid
			loc := locs[i]
			gi := d.group * 2
			if gi+1 >= len(loc) || loc[gi] < 0 {
				continue
			}
			start, end := loc[gi], loc[gi+1]
			orig := s[start:end]
			if strings.HasPrefix(orig, phPrefix) {
				continue // already redacted
			}
			counter[d.name]++
			ph := phPrefix + strings.ToUpper(d.name) + "_" + strconv.Itoa(counter[d.name]) + "]"
			repl[ph] = orig
			s = s[:start] + ph + s[end:]
		}
	}
	return s
}

// restoringWriter restores redacted placeholders back to their originals in the
// response stream, holding a small carry buffer so a placeholder split across
// writes is never emitted half-restored.
type restoringWriter struct {
	http.ResponseWriter
	rep    *strings.Replacer
	phs    []string
	maxLen int
	carry  []byte
}

func newRestoringWriter(w http.ResponseWriter, repl map[string]string) *restoringWriter {
	pairs := make([]string, 0, len(repl)*2)
	phs := make([]string, 0, len(repl))
	maxLen := 0
	for ph, orig := range repl {
		pairs = append(pairs, ph, orig)
		phs = append(phs, ph)
		if len(ph) > maxLen {
			maxLen = len(ph)
		}
	}
	return &restoringWriter{ResponseWriter: w, rep: strings.NewReplacer(pairs...), phs: phs, maxLen: maxLen}
}

func (rw *restoringWriter) Write(p []byte) (int, error) {
	buf := append(rw.carry, p...)
	hold := rw.holdLen(buf)
	split := len(buf) - hold
	if split > 0 {
		if _, err := rw.ResponseWriter.Write([]byte(rw.rep.Replace(string(buf[:split])))); err != nil {
			return 0, err
		}
	}
	rw.carry = append([]byte(nil), buf[split:]...)
	return len(p), nil
}

// holdLen returns how many trailing bytes to hold because they could be the
// start of a not-yet-complete placeholder.
func (rw *restoringWriter) holdLen(buf []byte) int {
	max := rw.maxLen
	if max > len(buf) {
		max = len(buf)
	}
	for L := max; L >= 1; L-- {
		suf := string(buf[len(buf)-L:])
		for _, ph := range rw.phs {
			if strings.HasPrefix(ph, suf) {
				return L
			}
		}
	}
	return 0
}

// flush emits any remaining carry (restored). Call once after the relay returns.
func (rw *restoringWriter) flush() {
	if len(rw.carry) > 0 {
		_, _ = rw.ResponseWriter.Write([]byte(rw.rep.Replace(string(rw.carry))))
		rw.carry = nil
	}
}

func (rw *restoringWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
