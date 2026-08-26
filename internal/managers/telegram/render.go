package telegram

import (
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
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

	if maxLen <= 0 {
		return []string{text}
	}

	chunks := make([]string, 0, utf8.RuneCountInString(text)/maxLen+1)
	var current strings.Builder

	active := make([]string, 0, 2)
	for _, token := range telegramHTMLTokens(text) {
		closers := closingTags(active)
		if current.Len() > 0 && runeLen(current.String())+runeLen(token)+runeLen(closers) > maxLen {
			current.WriteString(closers)
			chunks = append(chunks, current.String())
			current.Reset()

			for _, tag := range active {
				current.WriteString(tag)
			}
		}

		current.WriteString(token)
		active = updateActiveTags(active, token)
	}

	if current.Len() > 0 {
		current.WriteString(closingTags(active))
		chunks = append(chunks, current.String())
	}

	return chunks
}

func telegramHTMLTokens(text string) []string {
	tokens := make([]string, 0, len(text)/8)
	for text != "" {
		switch text[0] {
		case '<':
			if end := strings.IndexByte(text, '>'); end >= 0 {
				tokens = append(tokens, text[:end+1])
				text = text[end+1:]

				continue
			}
		case '&':
			if end := strings.IndexByte(text, ';'); end >= 0 {
				tokens = append(tokens, text[:end+1])
				text = text[end+1:]

				continue
			}
		}

		_, size := utf8.DecodeRuneInString(text)
		tokens = append(tokens, text[:size])
		text = text[size:]
	}

	return tokens
}

func updateActiveTags(active []string, token string) []string {
	if !strings.HasPrefix(token, "<") || !strings.HasSuffix(token, ">") {
		return active
	}

	if strings.HasPrefix(token, "</") {
		if len(active) > 0 {
			return active[:len(active)-1]
		}

		return active
	}

	if strings.HasPrefix(token, "<br") || strings.HasPrefix(token, "<!") {
		return active
	}

	return append(active, token)
}

func closingTags(active []string) string {
	var out strings.Builder

	for _, tag := range slices.Backward(active) {
		name := strings.TrimSuffix(strings.TrimPrefix(tag, "<"), ">")
		if i := strings.IndexByte(name, ' '); i >= 0 {
			name = name[:i]
		}

		out.WriteString("</")
		out.WriteString(name)
		out.WriteByte('>')
	}

	return out.String()
}

func runeLen(value string) int { return utf8.RuneCountInString(value) }

func stripHTMLToPlain(htmlText string) string {
	plain := reHTMLTag.ReplaceAllString(htmlText, "")
	return html.UnescapeString(plain)
}

func itoa(v int) string {
	return strconv.Itoa(v)
}
