//go:build !linux

package sandboxnet

// RunSetup is unavailable off Linux.
func RunSetup(SetupConfig) error { return ErrSetupUnsupported }
