package telegram

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

var (
	reCodeBlock  = regexp.MustCompile("(?s)```\\w*\\n(.*?)```")
	reInlineCode = regexp.MustCompile("`([^`]+)`")
	reBold       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic     = regexp.MustCompile(`\*([^*\n]+)\*`)
	reStrike     = regexp.MustCompile(`~~([^~]+)~~`)
	reHTMLTag    = regexp.MustCompile(`<[^>]+>`)
	reHeading    = regexp.MustCompile(`(?m)^(#{1,6})[ \t]+(.+?)[ \t]*$`)
)

// nul wraps protected-block placeholders; built via rune(0) to keep a real NUL out of source.
var nul = string(rune(0))

func escapeHTML(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

func textToTelegramHTML(text string) string {
	codeBlocks := make([]string, 0) // code fences and rendered tables — kept out of HTML escaping
	inlineCodes := make([]string, 0)

	result := reCodeBlock.ReplaceAllStringFunc(text, func(block string) string {
		m := reCodeBlock.FindStringSubmatch(block)
		code := ""

		if len(m) > 1 {
			code = strings.TrimSpace(m[1])
		}

		return stashBlock(&codeBlocks, "<pre>"+escapeHTML(code)+"</pre>")
	})

	result = reTable.ReplaceAllStringFunc(result, func(table string) string {
		return stashBlock(&codeBlocks, renderMarkdownTable(strings.TrimSpace(table)))
	})

	result = reInlineCode.ReplaceAllStringFunc(result, func(match string) string {
		m := reInlineCode.FindStringSubmatch(match)
		code := ""

		if len(m) > 1 {
			code = m[1]
		}

		placeholder := nul + "IC" + itoa(len(inlineCodes)) + nul
		inlineCodes = append(inlineCodes, "<code>"+escapeHTML(code)+"</code>")

		return placeholder
	})

	result = escapeHTML(result)
	result = applyHeadings(result)
	result = reBold.ReplaceAllString(result, "<b>$1</b>")
	result = reItalic.ReplaceAllString(result, "<i>$1</i>")
	result = reStrike.ReplaceAllString(result, "<s>$1</s>")

	for i, block := range codeBlocks {
		result = strings.ReplaceAll(result, nul+"CB"+itoa(i)+nul, block)
	}

	for i, code := range inlineCodes {
		result = strings.ReplaceAll(result, nul+"IC"+itoa(i)+nul, code)
	}

	return result
}

func stashBlock(blocks *[]string, content string) string {
	placeholder := nul + "CB" + itoa(len(*blocks)) + nul
	*blocks = append(*blocks, content)

	return placeholder
}

// applyHeadings renders markdown headings; h1/h2 add underline so they read louder than inline bold.
func applyHeadings(text string) string {
	return reHeading.ReplaceAllStringFunc(text, func(line string) string {
		m := reHeading.FindStringSubmatch(line)
		if len(m) < 3 {
			return line
		}

		if len(m[1]) <= 2 {
			return "<b><u>" + m[2] + "</u></b>"
		}

		return "<b>" + m[2] + "</b>"
	})
}

func splitMessageChunks(text string, maxLen int) []string {
	if text == "" {
		return []string{""}
	}

	lines := strings.Split(text, "\n")
	chunks := make([]string, 0)
	current := ""

	for _, line := range lines {
		if len(current)+len(line)+1 > maxLen && current != "" {
			chunks = append(chunks, current)
			current = ""
		}

		if len(line) > maxLen && current == "" {
			for i := 0; i < len(line); i += maxLen {
				end := min(i+maxLen, len(line))

				chunks = append(chunks, line[i:end])
			}

			continue
		}

		if current != "" {
			current += "\n"
		}

		current += line
	}

	if current != "" {
		chunks = append(chunks, current)
	}

	if len(chunks) == 0 {
		chunks = []string{text}
	}

	return chunks
}

func stripHTMLToPlain(htmlText string) string {
	plain := reHTMLTag.ReplaceAllString(htmlText, "")
	return html.UnescapeString(plain)
}

func itoa(v int) string {
	return strconv.Itoa(v)
}
