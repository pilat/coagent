//go:build !darwin && !linux

package bashsandbox

func runtimeDirectoryCandidates() []string { return nil }
func runtimeFileCandidates() []string      { return nil }
