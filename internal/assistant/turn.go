package assistant

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	phaseMain        = "main"
	phaseFinal       = "final"
	phaseConsolidate = "consolidate"
	phaseGuard       = "guard"
	effectOpen       = "open"
	effectSend       = "send"
	effectStop       = "stop"
	focusTextChars   = 200
	focusLiveItems   = 30
	aboutChars       = 200
)

var errFinalReply = errors.New("the reply is final; no new work in this phase")

// turn is one input's conversation; nothing in it has an effect before commit.
type turn struct {
	input      *sessionstore.InboxInput
	client     llm.Client
	now        time.Time
	today      string
	messages   []llmwire.Message
	recorded   int
	calls      []Call
	replies    []string
	memory     []*memoryItem
	focus      []*focusItem
	before     []Focus
	effects    []*effect
	reads      []Card
	opened     int
	notes      int
	items      int
	seq        int
	lastSearch int
	lastEdit   int
}

type memoryItem struct {
	Memory
	ref     string
	changed bool
	deleted bool
}

type focusItem struct {
	Focus
	ref     string
	changed bool
	deleted bool
}

// effect is a recorded do-tool call; target is the validated session of a send or stop.
type effect struct {
	kind    string
	handle  string
	target  Card
	project string
	about   string
	text    string
}

func newTurn(input *sessionstore.InboxInput, memory []Memory, focus []Focus, now time.Time) *turn {
	t := &turn{input: input, now: now, today: now.UTC().Format(time.DateOnly), before: focus}

	for _, note := range memory {
		t.memory = append(t.memory, &memoryItem{Memory: note, ref: fmt.Sprintf("m%d", note.ID)})
	}

	for _, item := range focus {
		item.Sessions = slices.Clone(item.Sessions)
		t.focus = append(t.focus, &focusItem{Focus: item, ref: fmt.Sprintf("f%d", item.ID)})
	}

	return t
}

func (t *turn) remember(text, until string) (string, error) {
	note := Memory{Text: strings.TrimSpace(text), Until: strings.TrimSpace(until)}
	if err := t.checkMemory(note, nil); err != nil {
		return "", err
	}

	t.notes++
	item := &memoryItem{Memory: note, ref: fmt.Sprintf("m-new-%d", t.notes), changed: true}
	t.memory = append(t.memory, item)

	return item.ref, nil
}

func (t *turn) rewriteMemory(ref, text, until string) error {
	item := t.findMemory(ref)
	if item == nil {
		return fmt.Errorf("no memory note %q", ref)
	}

	note := Memory{ID: item.ID, Text: strings.TrimSpace(text), Until: strings.TrimSpace(until)}
	if err := t.checkMemory(note, item); err != nil {
		return err
	}

	item.Text, item.Until, item.changed = note.Text, note.Until, true

	return nil
}

func (t *turn) forgetMemory(ref string) error {
	item := t.findMemory(ref)
	if item == nil {
		return fmt.Errorf("no memory note %q", ref)
	}

	item.deleted = true

	return nil
}

// checkMemory rejects an edit only when it grows the live total past the limit, so over-limit memory can shrink.
func (t *turn) checkMemory(note Memory, replaced *memoryItem) error {
	if note.Text == "" {
		return errors.New("a memory note needs text")
	}

	if chars := utf8.RuneCountInString(note.Text); chars > memoryNoteChars {
		return fmt.Errorf("a memory note holds at most %d characters, this one has %d", memoryNoteChars, chars)
	}

	if note.Until != "" {
		if _, err := time.Parse(time.DateOnly, note.Until); err != nil {
			return fmt.Errorf("until %q must be a date like 2026-10-25", note.Until)
		}
	}

	before := t.memoryChars()
	after := before

	if note.Live(t.today) {
		after += utf8.RuneCountInString(note.Text)
	}

	if replaced != nil && replaced.Live(t.today) {
		after -= utf8.RuneCountInString(replaced.Text)
	}

	if after > memoryTotalChars && after > before {
		return fmt.Errorf("memory is full (%d/%d): merge, shorten or remove notes first", before, memoryTotalChars)
	}

	return nil
}

func (t *turn) memoryChars() int {
	total := 0

	for _, item := range t.memory {
		if !item.deleted && item.Live(t.today) {
			total += utf8.RuneCountInString(item.Text)
		}
	}

	return total
}

func (t *turn) findMemory(ref string) *memoryItem {
	ref = strings.TrimSpace(ref)
	for _, item := range t.memory {
		if !item.deleted && (item.ref == ref || item.ref == "m"+ref) {
			return item
		}
	}

	return nil
}

func (t *turn) addFocus(text string, sessions []string) (string, error) {
	if err := checkFocusText(text); err != nil {
		return "", err
	}

	if live := len(t.liveFocus()); live >= focusLiveItems {
		return "", fmt.Errorf(
			"focus holds at most %d items: finish or remove some with done_focus first",
			focusLiveItems,
		)
	}

	t.items++
	item := &focusItem{
		Text: strings.TrimSpace(text), Sessions: sessions,
		ref: fmt.Sprintf("f-new-%d", t.items), changed: true,
	}
	t.focus = append(t.focus, item)

	return item.ref, nil
}

// updateFocus keeps the refs when sessions were omitted: dropping them silently ends the wait.
func (t *turn) updateFocus(ref, text string, sessions []string, keepSessions bool) error {
	item := t.findFocus(ref)
	if item == nil {
		return fmt.Errorf("no focus item %q", ref)
	}

	if err := checkFocusText(text); err != nil {
		return err
	}

	if !keepSessions {
		item.Sessions = sessions
	}

	item.Text, item.changed = strings.TrimSpace(text), true

	return nil
}

func (t *turn) doneFocus(ref string) error {
	item := t.findFocus(ref)
	if item == nil {
		return fmt.Errorf("no focus item %q", ref)
	}

	item.deleted = true

	return nil
}

func (t *turn) findFocus(ref string) *focusItem {
	ref = strings.TrimSpace(ref)
	for _, item := range t.focus {
		if !item.deleted && (item.ref == ref || item.ref == "f"+ref) {
			return item
		}
	}

	return nil
}

func (t *turn) liveFocus() []*focusItem {
	var live []*focusItem

	for _, item := range t.focus {
		if !item.deleted {
			live = append(live, item)
		}
	}

	return live
}

func (t *turn) openHandle(handle string) *effect {
	for _, e := range t.effects {
		if e.kind == effectOpen && e.handle == handle {
			return e
		}
	}

	return nil
}

// problems lists the bookkeeping the guard asks the model to fix.
func (t *turn) problems() []string {
	var problems []string

	for _, e := range t.effects {
		ref, name := e.handle, e.handle+" ("+e.project+")"
		if e.kind == effectSend && e.handle == "" {
			ref, name = strconv.FormatInt(e.target.ID, 10), fmt.Sprintf("%d %s", e.target.ID, e.target.Project)
		}

		if e.kind != effectStop && !t.referenced(ref) {
			problems = append(problems, fmt.Sprintf("%s to %s has no focus item: add one with focus(text, "+
				"sessions: [%q]) saying what to tell the user when it answers", e.kind, name, ref))
		}

		if e.kind == effectStop {
			problems = append(problems, t.stale(e.target.ID, "was stopped")...)
		}
	}

	kind, _ := t.input.Attributes["assistant_event"].(string)
	if id, ok := attributeID(t.input.Attributes["session_id"]); ok && (kind == "answered" || kind == "failed") {
		problems = append(problems, t.stale(id, kind)...)
	}

	if t.lastSearch > 0 && t.lastEdit < t.lastSearch {
		problems = append(problems, "you searched but wrote nothing down: record what you found, or that nothing "+
			"was found, as memory (remember with until for a temporary note) or as focus")
	}

	if total := t.memoryChars(); total > memoryTotalChars {
		problems = append(problems, fmt.Sprintf("memory is full (%d/%d): merge, shorten or remove notes",
			total, memoryTotalChars))
	}

	return problems
}

func (t *turn) referenced(ref string) bool {
	return slices.ContainsFunc(t.liveFocus(), func(item *focusItem) bool { return slices.Contains(item.Sessions, ref) })
}

// stale names focus items that waited on a session before this turn and were left untouched.
func (t *turn) stale(id int64, what string) []string {
	ref := strconv.FormatInt(id, 10)

	var problems []string

	for _, before := range t.before {
		if !slices.Contains(before.Sessions, ref) {
			continue
		}

		for _, item := range t.focus {
			if item.ID == before.ID && !item.changed && !item.deleted {
				problems = append(problems, fmt.Sprintf("session %d %s, but focus item %s that waits on it was not "+
					"updated: update_focus it with what is still open, or done_focus it", id, what, item.ref))
			}
		}
	}

	return problems
}

func checkFocusText(text string) error {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, "\r\n") {
		return errors.New("a focus item needs one line of text")
	}

	if chars := utf8.RuneCountInString(text); chars > focusTextChars {
		return fmt.Errorf("a focus item holds at most %d characters, this one has %d", focusTextChars, chars)
	}

	return nil
}
