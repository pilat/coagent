package sandboxpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// digest is the identity of one compiled policy. It covers project identity
// and every entry's path, action and mode, in order. Presence, kind and inode
// are deliberately excluded.
func (p Policy) digest() string {
	parts := make([]string, 0, 2+len(p.Entries))
	parts = append(parts,
		"project:"+p.ProjectRoot,
		fmt.Sprintf("project_id:%d", p.ProjectID),
	)

	for _, entry := range p.Entries {
		parts = append(parts, fmt.Sprintf(
			"entry:%s:%s:%s", entry.Path, entry.Action, entry.Mode,
		))
	}

	hash := sha256.Sum256([]byte(strings.Join(parts, "\n")))

	return hex.EncodeToString(hash[:])
}
