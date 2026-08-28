package configops

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/config"
)

const reservedCLIManagerID = "cli"

// ManagerPatch is a presence-aware mutation: nil fields preserve and non-nil fields replace.
// Whisper has Set because omission and explicit removal must remain distinct.
type ManagerPatch struct {
	ID                      string
	Driver                  *string
	Enabled                 *bool
	BotToken                *string
	APIURL                  *string
	AllowedUserIDs          *[]int64
	TargetChatID            *int64
	ServiceTopicName        *string
	ServiceTopicIconEmojiID *string
	SessionTopicIconEmojiID *string
	SendChunkDelayMS        *int
	PollTimeoutSec          *int
	Whisper                 WhisperPatch
}

// WhisperPatch distinguishes omission, explicit removal, and replacement.
type WhisperPatch struct {
	Set   bool
	Value *config.ManagerWhisperEntry
}

// SetManagerPatch creates or updates a manager from a presence-aware patch.
func SetManagerPatch(patch ManagerPatch) Op {
	return &setManagerPatch{patch: patch}
}

type setManagerPatch struct {
	patch   ManagerPatch
	summary string // computed during apply
}

func (o *setManagerPatch) Path() string { return "managers." + o.patch.ID }

func (o *setManagerPatch) Summary() string { return o.summary }

func (o *setManagerPatch) apply(draft *config.UnifiedConfig) error {
	if strings.TrimSpace(o.patch.ID) == "" {
		return errors.New("manager id is required")
	}

	if o.patch.ID == reservedCLIManagerID {
		return errors.New(`manager id "cli" is reserved`)
	}

	if o.patch.BotToken != nil {
		if err := checkCredential("bot_token", *o.patch.BotToken); err != nil {
			return err
		}
	}

	idx, existing := managerIndex(draft.Managers, o.patch.ID)

	if existing == nil {
		o.summary = fmt.Sprintf("add manager %q", o.patch.ID)

		return o.applyCreate(draft)
	}

	return o.applyUpdate(draft, idx, existing)
}

func (o *setManagerPatch) applyCreate(draft *config.UnifiedConfig) error {
	if o.patch.Driver == nil || *o.patch.Driver == "" {
		return errors.New("a new manager needs a driver")
	}

	if *o.patch.Driver != "telegram" {
		return fmt.Errorf("unsupported driver %q", *o.patch.Driver)
	}

	if o.patch.BotToken == nil || *o.patch.BotToken == "" {
		return errors.New("a new manager needs a bot_token reference")
	}

	entry := newManagerEntry(o.patch)

	draft.Managers = append(draft.Managers, entry)

	return nil
}

func (o *setManagerPatch) applyUpdate(draft *config.UnifiedConfig, idx int, current *config.ManagerEntry) error {
	if o.patch.Driver != nil && *o.patch.Driver != current.Driver {
		return fmt.Errorf("manager %q driver cannot change; create a new manager id", o.patch.ID)
	}

	if patchWouldChangeTarget(*current, o.patch) {
		return fmt.Errorf("manager %q forum target cannot change; create a new manager id", o.patch.ID)
	}

	before := *current

	entry := mergeManagerEntry(*current, o.patch)

	draft.Managers[idx] = entry

	changed := diffFields(before, entry)
	if len(changed) == 0 {
		o.summary = fmt.Sprintf("reapply manager %q (no config changes)", o.patch.ID)
	} else {
		o.summary = fmt.Sprintf("update manager %q (%s)", o.patch.ID, strings.Join(changed, ", "))
	}

	return nil
}
