package proxy

import (
	"regexp"
	"strconv"
	"strings"
)

// imageDataRe matches a JSON `"data":"<base64>"` member whose value is a run of
// at least 128 base64 characters (standard or URL-safe alphabet, padded or
// not). It is how an Anthropic image block's base64 source appears in the
// marshaled request. Go's regexp is RE2 (linear time), so large inputs are safe.
var imageDataRe = regexp.MustCompile(`"data":"[A-Za-z0-9+/_=-]{128,}"`)

// elideImageData replaces the value of every long base64 `"data":"..."` member
// in s (a marshaled request) with `"data":"[image data omitted: N bytes]"`, so
// the inspector does not store a truncated, useless base64 blob. Short data
// values and everything else are left untouched.
func elideImageData(s string) string {
	if !strings.Contains(s, `"data":"`) { // cheap pre-check: skip the regexp for text-only prompts
		return s
	}
	const wrapLen = len(`"data":""`)
	return imageDataRe.ReplaceAllStringFunc(s, func(m string) string {
		return `"data":"[image data omitted: ` + strconv.Itoa(len(m)-wrapLen) + ` bytes]"`
	})
}

// maxImageScanDepth caps how deep contentHasImage recurses into nested
// "content" arrays (tool_result inside tool_result ...), so a hostile request
// cannot force unbounded recursion.
const maxImageScanDepth = 8

// contentHasImage reports whether v contains an image block. v is a message
// "content", a "system" value, or a block slice, in either the Anthropic shape
// ("image") or the OpenAI shapes ("image_url", "input_image"). A block that
// carries its own "content" array (an Anthropic tool_result) is searched
// recursively, up to maxImageScanDepth levels. Plain-string content is never an
// image.
func contentHasImage(v interface{}) bool {
	return contentHasImageDepth(v, 0)
}

func contentHasImageDepth(v interface{}, depth int) bool {
	if depth > maxImageScanDepth {
		return false
	}
	arr, ok := v.([]interface{})
	if !ok {
		return false
	}
	for _, b := range arr {
		bm, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		switch t, _ := bm["type"].(string); t {
		case "image", "image_url", "input_image":
			return true
		}
		if contentHasImageDepth(bm["content"], depth+1) {
			return true
		}
	}
	return false
}

// messagesHaveImage reports whether any message's content contains an image
// block (see contentHasImage).
func messagesHaveImage(msgs []map[string]interface{}) bool {
	for _, m := range msgs {
		if contentHasImage(m["content"]) {
			return true
		}
	}
	return false
}
