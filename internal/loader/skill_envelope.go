package loader

import (
	"html"
	"regexp"
	"strings"
)

// SkillReceiptPrefix is the manager-visible activation receipt prefix for one
// skill. Presentation-only: it names the canonical skill and nothing else.
const SkillReceiptPrefix = "🔧 Activated skill: "

var batchCallHeaderPattern = regexp.MustCompile(`(?m)^=== [^\n]+ \(call \d+\) ===\n`)

// RenderedSkill is a canonical skill envelope found in conversation content.
type RenderedSkill struct {
	Name     string
	Envelope string
}

// ExtractRenderedSkill extracts a canonical skill envelope from transport-prefixed content.
func ExtractRenderedSkill(content string) (string, string, bool) {
	skills := ExtractRenderedSkills(content)
	if len(skills) == 0 {
		return "", "", false
	}

	return skills[0].Name, skills[0].Envelope, true
}

// ExtractRenderedSkills extracts every canonical skill envelope from conversation content.
func ExtractRenderedSkills(content string) []RenderedSkill {
	const (
		startMarker = "<skill>\n<name>"
		nameEnd     = "</name>"
		endMarker   = "</skill>"
	)

	var skills []RenderedSkill

	for _, segment := range renderedSkillSegments(content, startMarker) {
		start := strings.Index(segment, startMarker)
		if start < 0 {
			continue
		}

		nameStart := start + len(startMarker)
		relativeNameEnd := strings.Index(segment[nameStart:], nameEnd)

		if relativeNameEnd < 0 {
			continue
		}

		name := html.UnescapeString(segment[nameStart : nameStart+relativeNameEnd])
		if name == "" {
			continue
		}

		envelopeEnd := len(segment)
		if end := strings.LastIndex(segment[start:], endMarker); end >= 0 {
			envelopeEnd = start + end + len(endMarker)
		}

		skills = append(skills, RenderedSkill{Name: name, Envelope: segment[start:envelopeEnd]})
	}

	return skills
}

// SkillReceipt renders the manager-visible activation receipt for one skill.
func SkillReceipt(name string) string {
	return SkillReceiptPrefix + name
}

func renderedSkillSegments(content, startMarker string) []string {
	headers := batchCallHeaderPattern.FindAllStringIndex(content, -1)
	if len(headers) == 0 || strings.Index(content, startMarker) < headers[0][0] {
		return []string{content}
	}

	segments := make([]string, 0, len(headers))
	for i, header := range headers {
		if !strings.HasPrefix(content[header[0]:header[1]], "=== skill ") {
			continue
		}

		end := len(content)
		if i+1 < len(headers) {
			end = headers[i+1][0]
		}

		segments = append(segments, content[header[1]:end])
	}

	return segments
}
