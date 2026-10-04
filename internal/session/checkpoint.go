package session

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/tool"
)

// Host-owned checkpoint wrapper. The complete wrapper is what makes a row a
// recognizable previous summary; delimiter collisions in user content are
// accepted user-controlled content, not a security boundary.
const (
	compactionMarkOpen  = "[CONTEXT SUMMARY of older history — lossy; later verbatim messages are newer and take precedence on conflict]"
	compactionMarkClose = "[/CONTEXT SUMMARY]"
)

// compactionRequestFraction is the normal share of the window the complete
// summarizer request targets.
const compactionRequestFraction = 0.5

// checkpointPrefix describes the scaffolding a previous successful compaction
// left between the immutable header and the raw transcript.
type checkpointPrefix struct {
	summaryRowIdx int    // previous marked summary row, -1 when none
	prevSummary   string // model-authored text extracted from that row
	skillRowIdx   int    // previous host reattachment row, -1 when none
	rawStart      int    // first raw (non-scaffolding) row after the header
}

// tailLimit stages the tail constraints: the token floor with both low-water
// ceilings, then the ceilings alone (D2 — the byte ceiling beats the token
// floor), then nothing but legality (D3 — the tail is never empty, even when
// a single legal group breaches the ceiling).
type tailLimit struct {
	floor    bool
	ceilings bool
}

// selectTailSplitArgs carries one split-search walk's inputs; the parameter
// set outgrew a readable positional call at both call sites.
type selectTailSplitArgs struct {
	messages            []llmwire.Message
	base                int
	minTail             int
	tailCap             int
	requestBaseEstimate int
	window              int
	limit               tailLimit
	inputFraction       float64
}

// isMarkedSummary reports whether content carries the complete outer wrapper.
func isMarkedSummary(content string) bool {
	return strings.HasPrefix(content, compactionMarkOpen) && strings.HasSuffix(content, compactionMarkClose)
}

// parseMarkedSummary splits a marked summary row into its model-authored text
// and host-owned background section. Only the complete outer wrapper qualifies.
func parseMarkedSummary(content string) (string, string, bool) {
	if !isMarkedSummary(content) {
		return "", "", false
	}

	inner := content[len(compactionMarkOpen) : len(content)-len(compactionMarkClose)]
	inner = strings.TrimPrefix(inner, "\n\n")
	inner = strings.TrimSuffix(inner, "\n")

	backgroundIdx := strings.LastIndex(inner, sessionprompt.BackgroundSectionMarker)

	legacyIdx := strings.LastIndex(inner, legacyBackgroundSectionMarker)
	if legacyIdx > backgroundIdx {
		backgroundIdx = legacyIdx
	}

	if backgroundIdx >= 0 {
		return strings.TrimSuffix(inner[:backgroundIdx], "\n"), inner[backgroundIdx:], true
	}

	return inner, "", true
}

// renderMarkedSummary wraps model text in the host marker, with the host-owned
// active-background section inside the wrapper when present.
func renderMarkedSummary(modelText, background string) string {
	out := compactionMarkOpen + "\n\n" + modelText
	if background != "" {
		out += "\n" + background
	}

	return out + compactionMarkClose
}

// lastEnvelope returns the last canonical skill envelope in content, if any.
// Transport stamping may prefix the content, so extraction is not exactness.
func lastEnvelope(content string) (string, bool) {
	envs := loader.ExtractRenderedSkills(content)
	if len(envs) == 0 {
		return "", false
	}

	return envs[len(envs)-1].Envelope, true
}

// exactEnvelope reports whether content is exactly one canonical envelope —
// the shape host-authored reattachment rows have.
func exactEnvelope(content string) (string, bool) {
	env, ok := lastEnvelope(content)
	if !ok {
		return "", false
	}

	return env, strings.TrimSpace(content) == env
}

// parseCheckpointPrefix recognizes a previous compaction's scaffolding: the
// marked summary row after the header plus at most one exact skill reattachment
// right after it. Anything else is raw history.
func parseCheckpointPrefix(messages []llmwire.Message, headerSize int) checkpointPrefix {
	cp := checkpointPrefix{summaryRowIdx: -1, skillRowIdx: -1, rawStart: headerSize}

	if headerSize >= len(messages) {
		return cp
	}

	row := messages[headerSize]
	if row.Role != llmwire.RoleUser {
		return cp
	}

	modelText, _, ok := parseMarkedSummary(row.Content)
	if !ok {
		return cp
	}

	cp.summaryRowIdx = headerSize
	cp.prevSummary = modelText
	cp.rawStart = headerSize + 1

	if cp.rawStart < len(messages) {
		next := messages[cp.rawStart]
		if _, isEnvelope := exactEnvelope(next.Content); isEnvelope && next.Role == llmwire.RoleUser {
			cp.skillRowIdx = cp.rawStart
			cp.rawStart++
		}
	}

	return cp
}

// selectCurrentSkill returns the latest skill candidate in position order:
// rendered envelopes in role-user rows or in paired role-tool rows named
// `skill`. skip excludes one row (the previous summary row — its model text may
// quote an envelope). Failed calls and batch-shaped output never qualify.
func selectCurrentSkill(messages []llmwire.Message, from, skip int) (int, string) {
	last, envelope := -1, ""

	for i := from; i < len(messages); i++ {
		if i == skip {
			continue
		}

		msg := messages[i]

		switch {
		case msg.Role == llmwire.RoleUser:
			if env, ok := lastEnvelope(msg.Content); ok {
				last, envelope = i, env
			}
		case msg.Role == llmwire.RoleTool && msg.ToolName == tool.IDSkill:
			if env, ok := lastEnvelope(msg.Content); ok {
				last, envelope = i, env
			}
		}
	}

	return last, envelope
}

// compactionHeaderSize keeps the existing immutable-header rule: an opening
// system/AGENTS row reserves itself plus the opening task when present,
// otherwise only the first row is header.
func compactionHeaderSize(messages []llmwire.Message) int {
	if len(messages) == 0 {
		return 0
	}

	if messages[0].Role == llmwire.RoleSystem || strings.HasPrefix(messages[0].Content, agentsMDMessagePrefix) {
		return min(2, len(messages))
	}

	return 1
}

// validateCompactionHeader rejects a header carrying tool protocol fields: the
// header is never summarized and a tool row there could never pair.
func validateCompactionHeader(header []llmwire.Message) error {
	for _, msg := range header {
		if msg.Role == llmwire.RoleTool || msg.ToolCallID != "" || len(msg.ToolCalls) > 0 {
			return fmt.Errorf("compaction header row %q carries tool protocol fields", msg.Role)
		}
	}

	return nil
}

// headerFitsLocked reports whether compaction can converge at all: the header is
// never summarized and the system prompt rides along on every request.
func (s *Session) headerFitsLocked(headerSize int) bool {
	size := estimateTokens(s.ms.messages[:headerSize]) + estimateText(s.prompt.SystemPrompt())

	return size <= compactionCutoff(s.contextWindow())
}

// assembleCheckpointLocked builds the positioned replacement projection —
// header, marked summary, optional current skill, exact selected tail — plus
// the DBIDs of every row the checkpoint marks compacted. A carried reattachment
// row keeps its DBID and is only repositioned.
func (s *Session) assembleCheckpointLocked(
	cp checkpointPrefix,
	headerSize, split, candIdx int,
	candEnvelope string,
	summaryMsg llmwire.Message,
) ([]llmwire.Message, []int64, []int64) {
	messages := s.ms.messages
	rowIDs := s.ms.rowIDs

	carriedSkill := candIdx >= 0 && candIdx == cp.skillRowIdx

	var skillRow *llmwire.Message

	switch {
	case carriedSkill:
		skillRow = &messages[cp.skillRowIdx]
	case candEnvelope != "" && candIdx >= cp.rawStart && candIdx < split:
		skillRow = &llmwire.Message{Role: llmwire.RoleUser, Content: candEnvelope}
	}

	newMessages := make([]llmwire.Message, 0, headerSize+2+len(messages)-split)
	newRowIDs := make([]int64, 0, cap(newMessages))
	newMessages = append(newMessages, messages[:headerSize]...)
	newRowIDs = append(newRowIDs, rowIDs[:headerSize]...)
	newMessages = append(newMessages, summaryMsg)
	newRowIDs = append(newRowIDs, 0)

	if skillRow != nil {
		newMessages = append(newMessages, *skillRow)
		if carriedSkill {
			newRowIDs = append(newRowIDs, rowIDs[cp.skillRowIdx])
		} else {
			newRowIDs = append(newRowIDs, 0)
		}
	}

	newMessages = append(newMessages, messages[split:]...)
	newRowIDs = append(newRowIDs, rowIDs[split:]...)

	compactedIDs := make([]int64, 0, split-headerSize)

	for i := headerSize; i < split; i++ {
		if carriedSkill && i == cp.skillRowIdx {
			continue
		}

		if rowIDs[i] != 0 {
			compactedIDs = append(compactedIDs, rowIDs[i])
		}
	}

	return newMessages, newRowIDs, compactedIDs
}

// rawCutLegal requires the repaired projection field-equal to the raw suffix
// and pairing complete with unique call identities and result ownership: every
// tool group stays indivisible and every late result stays with its call.
func rawCutLegal(rawTail []llmwire.Message) bool {
	if len(rawTail) == 0 {
		return true
	}

	repaired := repairTranscript(rawTail)
	if len(repaired) != len(rawTail) {
		return false
	}

	for i := range repaired {
		if !rawMessagesEqual(repaired[i], rawTail[i]) {
			return false
		}
	}

	return validateRawGrouping(rawTail) == nil
}

// validateRawGrouping adds the session-scope checks beyond llm.ValidateToolPairing:
// unique non-empty assistant call ids, non-empty names and result ownership.
func validateRawGrouping(messages []llmwire.Message) error {
	callNames := make(map[string]string)

	for _, msg := range messages {
		if msg.Role != llmwire.RoleAssistant {
			continue
		}

		for _, tc := range msg.ToolCalls {
			if tc.ID == "" || tc.Name == "" {
				return fmt.Errorf("assistant tool_call missing id or name (name=%q)", tc.Name)
			}

			if _, dup := callNames[tc.ID]; dup {
				return fmt.Errorf("duplicate tool_call id %q across assistant rows", tc.ID)
			}

			callNames[tc.ID] = tc.Name
		}
	}

	seen := make(map[string]bool)

	for _, msg := range messages {
		if msg.Role != llmwire.RoleTool || msg.ToolCallID == "" {
			continue
		}

		if seen[msg.ToolCallID] {
			return fmt.Errorf("duplicate tool result for call id %q", msg.ToolCallID)
		}

		seen[msg.ToolCallID] = true

		if name := callNames[msg.ToolCallID]; msg.ToolName != "" && name != msg.ToolName {
			return fmt.Errorf("tool result for %q owned by %q, stored tool name %q", msg.ToolCallID, name, msg.ToolName)
		}
	}

	return nil
}

// summarizerBaseEstimateLocked estimates everything the summarizer request
// carries besides the replayed transcript prefix: system prompt, final
// instruction, focus and the active tool schemas. Every byte participates in
// the request bound.
func (s *Session) summarizerBaseEstimateLocked() int {
	schemas := tool.ToSchemas(s.registry.List())
	if s.loopDetector.forceTextOnly {
		schemas = nil
	}

	base := s.prompt.SystemPrompt() +
		compactionInstructionMessage(s.focusSection()).Content

	return estimateText(base) + estimateSchemas(schemas)
}

// tailLevels is the staged relaxation order every split search walks.
func tailLevels() []tailLimit {
	return []tailLimit{{floor: true, ceilings: true}, {ceilings: true}, {}}
}

// minTailTokens is the token floor the tail should meet: a tenth of the window,
// degrading to half the raw history estimate when less history exists (D3).
func minTailTokens(messages []llmwire.Message, base, window int) int {
	return min(window/10, estimateTokens(messages[base:])/2)
}

// selectCheckpointSplit picks the maximal oldest raw prefix whose repaired
// native projection plus instruction and schemas fits the request bound,
// while the verbatim tail keeps the minimum tail estimate. The request
// estimate is not monotone in the split — a repair stub can exceed the real
// result it replaces — so every candidate is measured exactly. completionPin
// is a transcript position no legal split may cross: the pending completion
// candidate and its nudge stay verbatim tail rows.
func selectCheckpointSplit(
	messages []llmwire.Message,
	cp checkpointPrefix,
	requestBaseEstimate int,
	window int,
	completionPin int,
) (int, bool) {
	base := cp.rawStart
	if base >= len(messages) {
		return 0, false
	}

	minTail := minTailTokens(messages, base, window)

	// The pinned pair's first row defines the hard tail boundary; before it
	// sits the nudge, and past it nothing may summarize. A candidate at index
	// 0 is outside every legal raw range, so pin=0 stays the unpinned marker.
	tailCap := len(messages)
	if completionPin > base {
		tailCap = completionPin
	}

	// The 50% target is soft: the fallback rerun under the ordinary 85% input
	// ceiling keeps mandatory request input from failing an otherwise legal cut.
	for _, fraction := range []float64{compactionRequestFraction, llmwire.ContextInputFraction} {
		for _, limit := range tailLevels() {
			args := selectTailSplitArgs{
				messages: messages, base: base, minTail: minTail, tailCap: tailCap,
				requestBaseEstimate: requestBaseEstimate, window: window, limit: limit, inputFraction: fraction,
			}
			if split, ok := selectTailSplit(args); ok {
				return split, true
			}
		}
	}

	return 0, false
}

// selectTailSplit scans from the largest head down, returning the first
// candidate whose tail satisfies the staged limits, is a legal raw cut, and
// whose native prefix plus instruction fits the bound. The loop starts at
// len-1, not len: the split never summarizes the whole raw range. tailCap
// bounds every split at or before the pinned completion rows.
func selectTailSplit(args selectTailSplitArgs) (int, bool) {
	bound := int(args.inputFraction * float64(args.window))

	split := len(args.messages) - 1
	if args.tailCap > 0 && args.tailCap < split {
		split = args.tailCap
	}

	for ; split > args.base; split-- {
		if args.limit.floor && estimateTokens(args.messages[split:]) < args.minTail {
			continue
		}

		if args.limit.ceilings {
			if totalBytes, count := imagePressure(
				args.messages[split:],
			); totalBytes > imageBytesLowWater ||
				count > imageCountLowWater {
				continue
			}
		}

		if !rawCutLegal(args.messages[split:]) {
			continue
		}

		if args.requestBaseEstimate+estimateTokens(repairTranscript(args.messages[:split])) > bound {
			continue
		}

		return split, true
	}

	return 0, false
}

// rawMessagesEqual compares the fields repair may touch, byte-for-byte.
func rawMessagesEqual(a, b llmwire.Message) bool {
	if a.Role != b.Role || a.Content != b.Content ||
		a.ToolCallID != b.ToolCallID || a.ToolName != b.ToolName ||
		a.ReasoningContent != b.ReasoningContent ||
		!bytes.Equal(a.ReasoningRaw, b.ReasoningRaw) ||
		a.CostUSD != b.CostUSD ||
		!slices.Equal(a.Images, b.Images) {
		return false
	}

	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}

	for i := range a.ToolCalls {
		if a.ToolCalls[i].ID != b.ToolCalls[i].ID || a.ToolCalls[i].Name != b.ToolCalls[i].Name ||
			!bytes.Equal(a.ToolCalls[i].Arguments, b.ToolCalls[i].Arguments) {
			return false
		}
	}

	return true
}
