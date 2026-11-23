package localapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

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

func authenticate(clients []*client, next http.Handler) http.Handler {
	errorWriter := connecthttp.NewErrorWriter()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		author, err := authenticateHeader(clients, r.Header.Get("Authorization"))
		if err != nil {
			if errorWriter.IsSupported(r) {
				_ = errorWriter.Write(w, r, err)
				return
			}

			http.Error(w, err.Message(), http.StatusUnauthorized)

			return
		}

		// Connect copies headers and retains the request, so remove the credential first.
		r.Header.Del("Authorization")
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, author)))
	})
}

func authenticateHeader(clients []*client, header string) (*client, *connect.Error) {
	token, present := strings.CutPrefix(header, "Bearer ")
	if !present || token == "" {
		return nil, connect.Errorf(connect.CodeUnauthenticated, "missing bearer token")
	}

	hash := sha256.Sum256([]byte(token))
	var author *client

	for _, candidate := range clients {
		if subtle.ConstantTimeCompare(hash[:], candidate.hash[:]) == 1 {
			author = candidate
		}
	}

	if author == nil {
		return nil, connect.Errorf(connect.CodeUnauthenticated, "invalid bearer token")
	}

	return author, nil
}

func clientFrom(ctx context.Context) (*client, error) {
	author, ok := ctx.Value(clientKey{}).(*client)
	if !ok {
		return nil, connect.Errorf(connect.CodeUnauthenticated, "missing bearer token")
	}

	return author, nil
}
