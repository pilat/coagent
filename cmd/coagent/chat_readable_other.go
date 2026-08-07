//go:build !unix

package main

import "time"

// waitReadable cannot poll here, so the line read blocks and a secret request is
// noticed only once that line completes — and that line is what answers it.
func waitReadable(_ uintptr, _ time.Duration) (bool, error) { return true, nil }
