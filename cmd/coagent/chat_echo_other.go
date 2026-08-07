//go:build !linux && !darwin

package main

import "os"

// echoState carries nothing here: with no terminal mode to reconfigure, a masked
// prompt is read the way a chat line is, and the value is never echoed back.
type echoState struct{}

func maskEcho(*os.File) (echoState, error) { return echoState{}, nil }

func unmaskEcho(*os.File, echoState, bool) error { return nil }
