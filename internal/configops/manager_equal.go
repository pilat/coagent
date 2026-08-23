package configops

import "github.com/pilat/coagent/internal/config"

func boolPtrEqual(a, b *bool) bool {
	if (a == nil) != (b == nil) {
		return false
	}

	return a == nil || *a == *b
}

func int64PtrEqual(a, b *int64) bool {
	if (a == nil) != (b == nil) {
		return false
	}

	return a == nil || *a == *b
}

func whisperEqual(a, b *config.ManagerWhisperEntry) bool {
	if (a == nil) != (b == nil) {
		return false
	}

	return a == nil || (a.Provider == b.Provider && a.Model == b.Model)
}

func managerSlicesEqual[T comparable](a, b []T) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
