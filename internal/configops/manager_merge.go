package configops

import "github.com/pilat/coagent/internal/config"

func newManagerEntry(patch ManagerPatch) config.ManagerEntry {
	on := true

	entry := config.ManagerEntry{ID: patch.ID, Enabled: &on}
	if patch.Driver != nil {
		entry.Driver = *patch.Driver
	}

	return mergeManagerEntry(entry, patch)
}

func mergeManagerEntry(entry config.ManagerEntry, patch ManagerPatch) config.ManagerEntry {
	if patch.Enabled != nil {
		entry.Enabled = patch.Enabled
	}

	if patch.BotToken != nil && *patch.BotToken != "" {
		entry.BotToken = *patch.BotToken
	}

	if patch.APIURL != nil {
		entry.APIURL = *patch.APIURL
	}

	if patch.AllowedUserIDs != nil {
		entry.AllowedUserIDs = *patch.AllowedUserIDs
	}

	if patch.TargetChatID != nil {
		entry.TargetChatID = patch.TargetChatID
	}

	if patch.ServiceTopicName != nil {
		entry.ServiceTopicName = *patch.ServiceTopicName
	}

	if patch.ServiceTopicIconEmojiID != nil {
		entry.ServiceTopicIconEmojiID = *patch.ServiceTopicIconEmojiID
	}

	if patch.SessionTopicIconEmojiID != nil {
		entry.SessionTopicIconEmojiID = *patch.SessionTopicIconEmojiID
	}

	if patch.SendChunkDelayMS != nil {
		entry.SendChunkDelayMS = *patch.SendChunkDelayMS
	}

	if patch.PollTimeoutSec != nil {
		entry.PollTimeoutSec = *patch.PollTimeoutSec
	}

	if patch.Whisper.Set {
		entry.Whisper = patch.Whisper.Value
	}

	return entry
}
