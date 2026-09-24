package proxy

import (
	"encoding/json"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// sparseVec is an L2-normalized sparse term vector (hashed term → weight).
type sparseVec map[uint64]float32

// embed turns text into an L2-normalized sparse term-frequency vector over word
// unigrams + bigrams (hashed). It is purely local — no network, no model, no
// dependencies — so it preserves the single-binary, offline guarantee.
func embed(text string) sparseVec {
	toks := tokenize(text)
	if len(toks) == 0 {
		return nil
	}
	v := sparseVec{}
	v[hash64(toks[0])]++
	for i := 1; i < len(toks); i++ {
		v[hash64(toks[i])]++
		v[hash64(toks[i-1]+" "+toks[i])]++ // bigram
	}
	var sum float64
	for _, w := range v {
		sum += float64(w) * float64(w)
	}
	if sum == 0 {
		return nil
	}
	norm := float32(1 / math.Sqrt(sum))
	for k := range v {
		v[k] *= norm
	}
	return v
}

// cosine is the cosine similarity of two unit vectors (their dot product).
func cosine(a, b sparseVec) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(b) < len(a) {
		a, b = b, a
	}
	var dot float64
	for k, av := range a {
		if bv, ok := b[k]; ok {
			dot += float64(av) * float64(bv)
		}
	}
	return dot
}

func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// promptText extracts the concatenated system + message text, whether the
// request declares tools, and whether it contains any image content block,
// from either an Anthropic or OpenAI request body.
func promptText(body []byte) (text string, hasTools bool, hasImages bool, ok bool) {
	var m map[string]interface{}
	if json.Unmarshal(body, &m) != nil {
		return "", false, false, false
	}
	if t, exists := m["tools"]; exists {
		if arr, isArr := t.([]interface{}); isArr && len(arr) > 0 {
			hasTools = true
		}
	}
	var sb strings.Builder
	collectText(m["system"], &sb)
	if hasImageBlock(m["system"]) {
		hasImages = true
	}
	if msgs, isArr := m["messages"].([]interface{}); isArr {
		for _, mm := range msgs {
			if mp, isMap := mm.(map[string]interface{}); isMap {
				collectText(mp["content"], &sb)
				if hasImageBlock(mp["content"]) {
					hasImages = true
				}
			}
		}
	}
	text = strings.TrimSpace(sb.String())
	return text, hasTools, hasImages, text != ""
}

// hasImageBlock reports whether v (a "system" field or a message's "content"
// field) contains any content block whose "type" is "image". It walks the
// same []interface{}-of-map[string]interface{} shape collectText already
// walks, as a sibling scan rather than a second decode.
func hasImageBlock(v interface{}) bool {
	arr, ok := v.([]interface{})
	if !ok {
		return false
	}
	for _, b := range arr {
		if bm, ok := b.(map[string]interface{}); ok {
			if t, _ := bm["type"].(string); t == "image" {
				return true
			}
		}
	}
	return false
}

func collectText(v interface{}, sb *strings.Builder) {
	switch t := v.(type) {
	case string:
		sb.WriteString(t)
		sb.WriteByte(' ')
	case []interface{}:
		for _, b := range t {
			if bm, ok := b.(map[string]interface{}); ok {
				if s, ok := bm["text"].(string); ok {
					sb.WriteString(s)
					sb.WriteByte(' ')
				}
			}
		}
	}
}
