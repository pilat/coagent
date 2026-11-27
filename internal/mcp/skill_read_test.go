package mcp

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadSkillFile_VerifiesSizeAndDigest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents []any
		size     int64
		digest   string
		want     string
	}{
		{name: "text", contents: []any{map[string]any{"uri": "skill://pdf/SKILL.md", "text": "abc"}}, size: 3},
		{name: "blob", contents: []any{map[string]any{"uri": "skill://pdf/SKILL.md", "blob": base64.StdEncoding.EncodeToString([]byte("abc"))}}, size: 3},
		{name: "size", contents: []any{map[string]any{"uri": "skill://pdf/SKILL.md", "text": "abc"}}, size: 4, want: "failed verification: 3 bytes, listed 4"},
		{name: "digest", contents: []any{map[string]any{"uri": "skill://pdf/SKILL.md", "text": "abc"}}, size: 3, digest: "sha256:bad", want: "failed verification: sha256 does not match the listing"},
		{name: "server error", want: "resources/read: invalid params: missing fixture"},
		{name: "wrong uri", contents: []any{map[string]any{"uri": "skill://other/SKILL.md", "text": "abc"}}, want: "resources/read returned no content for this URI"},
		{name: "malformed blob", contents: []any{map[string]any{"uri": "skill://pdf/SKILL.md", "blob": "!"}}, want: "decode blob:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dir := skillsClient(t, true, false)
			uri := "skill://pdf/SKILL.md"
			if tc.contents != nil {
				name := fmt.Sprintf("read/%x.json", sha256.Sum256([]byte(uri)))
				name = name[:len("read/")+16] + ".json"
				writeSkillFixture(t, dir, name, map[string]any{"contents": tc.contents})
			}
			if tc.digest == "" {
				tc.digest = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("abc")))
			}
			data, err := c.ReadSkillFile(t.Context(), SkillFile{URI: uri, Size: tc.size, Digest: tc.digest})
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			} else {
				require.NoError(t, err)
				require.Equal(t, []byte("abc"), data)
			}
		})
	}
}
