package sandboxnet

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const sweepListing = `table ip filter
table ip nat
table inet coagent_cag06771360af77
table inet coagent_caga944e2ba28e4
table ip6 filter
`

func TestStaleHostTables_KeepsTheTablesWhoseInterfaceIsStillThere(t *testing.T) {
	live := func(link string) bool { return link == "caga944e2ba28e4" }

	assert.Equal(t, []string{"cag06771360af77"}, staleHostTables(sweepListing, live))
}

func TestStaleHostTables_LeavesForeignTablesAlone(t *testing.T) {
	listing := sweepListing + "table inet firewall\ntable inet coagent\n"

	stale := staleHostTables(listing, func(string) bool { return true })
	assert.Empty(t, stale, "a present generation and another owner's tables are untouched")
}

// The name reaches nft as part of a delete command, so anything this package
// could not have minted is refused rather than run.
func TestStaleHostTables_RefusesANameItCouldNotHaveMinted(t *testing.T) {
	listing := "table inet coagent_cag00; add rule x\ntable inet coagent_CAG01\n" +
		"table inet coagent_notours\ntable inet coagent_\n"

	assert.Empty(t, staleHostTables(listing, func(string) bool { return false }))
}

func TestStaleHostTables_SelectsEveryAbandonedGeneration(t *testing.T) {
	stale := staleHostTables(sweepListing, func(string) bool { return false })

	assert.Equal(t, []string{"cag06771360af77", "caga944e2ba28e4"}, stale)
}

func TestDeleteHostTable_MatchesTheNameTheRulesUse(t *testing.T) {
	assert.Equal(t, "delete table inet coagent_cag01\n", deleteHostTable("cag01"))
}
