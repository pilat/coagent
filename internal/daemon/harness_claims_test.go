package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

// drainScenarioClaims drains every output the scenario's manager owns through
// the production controller and acknowledges each claim exactly like the real
// manager would. Under -update-traces it also sanitizes the resulting
// OutputClaimData sequence into the shared fixture, so the telegram half can
// replay the recorded claims through its production transport. The drain runs
// in assert mode too: ack-triggered readiness events belong to the trace.
func drainScenarioClaims(t *testing.T, name string, controller controllerapi.OutputQueueController) {
	t.Helper()

	path := harnessTracePath(name)
	file := harnessTraceFile{SourceTest: t.Name()}
	if *updateHarnessTraces {
		if data, err := os.ReadFile(path); err == nil {
			require.NoError(t, json.Unmarshal(data, &file))
			file.SourceTest = t.Name()
		}

		// One drain corresponds to one scenario run: recorded claims are
		// replaced, never accumulated across recording passes.
		file.Claims = nil
	}

	receipts := map[string]string{}
	placeholderSeq := 0

	for {
		claim, err := controller.ClaimOutput(t.Context())
		if errors.Is(err, controllerapi.ErrNoOutput) {
			break
		}

		require.NoError(t, err, "production claim must succeed while recording")

		if *updateHarnessTraces {
			recorded := harnessTraceClaim{
				Type:                         claim.Type,
				Content:                      normalizeClaimContent(claim.Content),
				Attributes:                   sanitizeClaimAttributes(t, claim.Attributes),
				SourceKey:                    claim.SourceKey,
				ModelInputGeneration:         claim.ModelInputGeneration,
				PreviousMessageType:          claim.PreviousMessageType,
				PreviousModelInputGeneration: claim.PreviousModelInputGeneration,
				ReleasesInput:                claim.ReleasesInput,
			}
			for _, raw := range previousMessageIDs(claim) {
				placeholder, ok := receipts[raw]
				if !ok {
					placeholderSeq++
					placeholder = fmt.Sprintf("%s%d>", messagePlaceholderPrefix, placeholderSeq)
					receipts[raw] = placeholder
				}

				recorded.PreviousMessageIDs = append(recorded.PreviousMessageIDs, placeholder)
			}
			file.Claims = append(file.Claims, recorded)
		}

		if claim.Type == controllerapi.OutputMessageReplaceable ||
			claim.Type == controllerapi.OutputMessagePersistent {
			require.NoError(t, controller.AckOutput(t.Context(), controllerapi.OutputAckData{
				ID: claim.ID, AttemptID: claim.AttemptID,
				MessageIDs: []string{fmt.Sprintf("recorded-%d", claim.ID)},
			}), "production ack must succeed while recording")
		} else {
			require.NoError(t, controller.AckOutput(t.Context(), controllerapi.OutputAckData{
				ID: claim.ID, AttemptID: claim.AttemptID,
			}))
		}
	}

	if *updateHarnessTraces {
		writeHarnessTrace(t, path, file)
	}
}

// normalizeClaimContent keeps golden claims deterministic: wall time is the
// only fragment inside card content that varies between runs.
func normalizeClaimContent(content string) string {
	return elapsedPattern.ReplaceAllString(content, "⏱ <elapsed>")
}

// sanitizeClaimAttributes keeps only deterministic host metadata; progress
// revisions hash wall-clock observations and the work directory embeds a
// temporary path — neither carries conversation meaning.
func sanitizeClaimAttributes(t *testing.T, attributes map[string]any) map[string]any {
	t.Helper()

	sanitized := make(map[string]any, len(attributes))
	for key, value := range attributes {
		if key == "progress_revision" {
			continue
		}

		if key == "work_dir" {
			sanitized[key] = workDirPlaceholder

			continue
		}

		sanitized[key] = value
	}

	if len(sanitized) == 0 {
		return nil
	}

	return sanitized
}

func previousMessageIDs(claim *controllerapi.OutputClaimData) []string {
	values, ok := claim.PreviousMessageAttributes["message_ids"].([]any)
	if !ok {
		return nil
	}

	ids := make([]string, 0, len(values))
	for _, value := range values {
		id, ok := value.(string)
		if !ok {
			continue
		}

		ids = append(ids, id)
	}

	return ids
}
