package localapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToken_HashMatchesMintedToken(t *testing.T) {
	t.Parallel()
	first, err := NewToken()
	require.NoError(t, err)
	second, err := NewToken()
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
	for _, token := range []string{first, second} {
		require.True(t, strings.HasPrefix(token, "coagent_"))
		require.Len(t, token, len("coagent_")+43)
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "coagent_"))
		require.NoError(t, err)
		assert.Len(t, decoded, 32)
		hash := sha256.Sum256([]byte(token))
		assert.Equal(t, hex.EncodeToString(hash[:]), TokenHash(token))
		assert.Regexp(t, `^[0-9a-f]{64}$`, TokenHash(token))
	}
}
