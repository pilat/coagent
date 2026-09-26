//go:build linux

package sandboxnet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// SweepStaleRules removes the host rules of generations that no longer exist.
// Retirement deletes them normally; a daemon killed outright never runs it, and
// the kernel does not reclaim host rules the way it reclaims namespaces and
// links. Call it before the first generation of a run; it reports the count.
func SweepStaleRules(ctx context.Context) (int, error) {
	listing, err := listHostTables(ctx)
	if err != nil {
		return 0, err
	}

	stale := staleHostTables(listing, linkPresent)
	if len(stale) == 0 {
		return 0, nil
	}

	var removals strings.Builder
	for _, link := range stale {
		removals.WriteString(deleteHostTable(link))
	}

	if err := applyRouteRules(ctx, -1, removals.String()); err != nil {
		return 0, err
	}

	return len(stale), nil
}

func listHostTables(ctx context.Context) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, routeCommandTimeout)
	defer cancel()

	output, err := exec.CommandContext(commandCtx, "nft", "list", "tables").Output()
	if err != nil {
		return "", fmt.Errorf("list sandbox rule tables: %w", err)
	}

	return string(output), nil
}

func linkPresent(name string) bool {
	_, err := os.Stat("/sys/class/net/" + name)

	return err == nil
}
