package sessionbuild

import (
	"testing"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/tool/builtin"
)

type imageAuthorizerClient struct {
	llm.Client
	authorizer llm.ImageAuthorizer
}

func (c *imageAuthorizerClient) SetImageAuthorizer(authorizer llm.ImageAuthorizer) {
	c.authorizer = authorizer
}

func TestAttachImageAuthorizerWithoutAccessPreservesNil(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		stack *builtin.Stack
	}{
		{name: "nil stack"},
		{name: "empty stack", stack: &builtin.Stack{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := &imageAuthorizerClient{}
			authorizer := attachImageAuthorizer(client, tc.stack)
			// require.Nil also accepts an interface holding a nil pointer.
			if authorizer != nil || client.authorizer != nil {
				t.Fatalf("missing access must remain nil: session %T, client %T", authorizer, client.authorizer)
			}
		})
	}
}
