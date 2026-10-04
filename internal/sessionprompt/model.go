package sessionprompt

// ModelSection carries the model identity and its prompt policy.
type ModelSection struct {
	ID, Text     string
	NativeSearch bool
}
