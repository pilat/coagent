package localapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

type client struct {
	hash    [32]byte
	control controllerapi.Controller
	chat    controllerapi.ChatFocusController
	lineage controllerapi.LineageController
}

type clientKey struct{}

func bindClient(entry config.APIClientEntry, controllers controllerapi.ManagerControllerFactory) (*client, error) {
	raw, err := hex.DecodeString(entry.TokenSHA256)
	if err != nil {
		return nil, fmt.Errorf("decode token hash for api client %q: %w", entry.ID, err)
	}

	if len(raw) != sha256.Size {
		return nil, fmt.Errorf("api client %q has invalid token hash length", entry.ID)
	}

	control := controllers.ForManager(entry.ID)

	chat, ok := control.(controllerapi.ChatFocusController)
	if !ok {
		return nil, fmt.Errorf("controller for api client %q lacks ChatFocusController", entry.ID)
	}

	lineage, ok := control.(controllerapi.LineageController)
	if !ok {
		return nil, fmt.Errorf("controller for api client %q lacks LineageController", entry.ID)
	}

	return &client{hash: [32]byte(raw), control: control, chat: chat, lineage: lineage}, nil
}

func authenticate(clients []*client) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info, ok := connect.CallInfoForServerContext(ctx)
			if !ok {
				return connect.Errorf(connect.CodeUnauthenticated, "missing bearer token")
			}

			token, present := strings.CutPrefix(info.RequestHeader().Get("Authorization"), "Bearer ")
			if !present || token == "" {
				return connect.Errorf(connect.CodeUnauthenticated, "missing bearer token")
			}

			hash := sha256.Sum256([]byte(token))
			var author *client

			for _, candidate := range clients {
				if subtle.ConstantTimeCompare(hash[:], candidate.hash[:]) == 1 {
					author = candidate
				}
			}

			if author == nil {
				return connect.Errorf(connect.CodeUnauthenticated, "invalid bearer token")
			}

			return next(context.WithValue(ctx, clientKey{}, author), spec, stream)
		}
	}
}

func clientFrom(ctx context.Context) (*client, error) {
	author, ok := ctx.Value(clientKey{}).(*client)
	if !ok {
		return nil, connect.Errorf(connect.CodeUnauthenticated, "missing bearer token")
	}

	return author, nil
}
