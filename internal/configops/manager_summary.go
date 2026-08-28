package configops

import "github.com/pilat/coagent/internal/config"

var fieldOrder = []string{
	"driver", "enabled", "bot_token", "api_url", "allowed_user_ids", "target_chat_id",
	"service_topic_name", "service_topic_icon_emoji_id", "session_topic_icon_emoji_id",
	"send_chunk_delay_ms", "poll_timeout_sec", "whisper",
}

func patchWouldChangeTarget(current config.ManagerEntry, patch ManagerPatch) bool {
	if patch.TargetChatID != nil {
		if current.TargetChatID == nil || *current.TargetChatID != *patch.TargetChatID {
			return true
		}
	}

	if current.TargetChatID == nil && patch.AllowedUserIDs != nil {
		return len(current.AllowedUserIDs) != 1 || len(*patch.AllowedUserIDs) != 1 ||
			current.AllowedUserIDs[0] != (*patch.AllowedUserIDs)[0]
	}

	return false
}

func managerIndex(managers []config.ManagerEntry, id string) (int, *config.ManagerEntry) {
	for i := range managers {
		if managers[i].ID == id {
			return i, &managers[i]
		}
	}

	return -1, nil
}

func diffFields(before, after config.ManagerEntry) []string {
	var changed []string

	for _, name := range fieldOrder {
		if fieldChanged(name, before, after) {
			changed = append(changed, name)
		}
	}

	return changed
}

func fieldChanged(name string, before, after config.ManagerEntry) bool {
	switch name {
	case "driver":
		return before.Driver != after.Driver
	case "enabled":
		return !boolPtrEqual(before.Enabled, after.Enabled)
	case "bot_token":
		return before.BotToken != after.BotToken
	case "api_url":
		return before.APIURL != after.APIURL
	case "allowed_user_ids":
		return !managerSlicesEqual(before.AllowedUserIDs, after.AllowedUserIDs)
	case "target_chat_id":
		return !int64PtrEqual(before.TargetChatID, after.TargetChatID)
	case "service_topic_name":
		return before.ServiceTopicName != after.ServiceTopicName
	case "service_topic_icon_emoji_id":
		return before.ServiceTopicIconEmojiID != after.ServiceTopicIconEmojiID
	case "session_topic_icon_emoji_id":
		return before.SessionTopicIconEmojiID != after.SessionTopicIconEmojiID
	case "send_chunk_delay_ms":
		return before.SendChunkDelayMS != after.SendChunkDelayMS
	case "poll_timeout_sec":
		return before.PollTimeoutSec != after.PollTimeoutSec
	case "whisper":
		return !whisperEqual(before.Whisper, after.Whisper)
	default:
		return false
	}
}
