//go:build !linux

package bashsandbox

func runtimeDirectoryCandidates() []string { return nil }
func runtimeFileCandidates() []string      { return nil }
