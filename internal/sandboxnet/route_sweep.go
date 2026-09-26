package sandboxnet

import (
	"fmt"
	"strings"
)

// linkNamePrefix begins every generation interface name; the rest is hex.
const linkNamePrefix = "cag"

// deleteHostTable renders the removal of one generation's host rules.
func deleteHostTable(link string) string {
	return fmt.Sprintf("delete table inet %s%s\n", hostTablePrefix, link)
}

// staleHostTables picks the generation tables whose interface is gone. The
// kernel reclaims namespaces and links when the daemon dies, but host rules
// outlive it, so a crash leaves exactly these behind. A live generation owns
// its interface and is therefore never selected.
func staleHostTables(listing string, present func(string) bool) []string {
	var stale []string

	for line := range strings.SplitSeq(listing, "\n") {
		link, found := strings.CutPrefix(strings.TrimSpace(line), "table inet "+hostTablePrefix)
		if !found || !isGenerationLink(link) || present(link) {
			continue
		}

		stale = append(stale, link)
	}

	return stale
}

// isGenerationLink rejects any name this package did not mint. The name reaches
// nft as part of a command, so an unexpected one is refused rather than run.
func isGenerationLink(name string) bool {
	hex, found := strings.CutPrefix(name, linkNamePrefix)
	if !found || hex == "" {
		return false
	}

	for _, digit := range hex {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return false
		}
	}

	return true
}
