package llm

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// newGoogleTokenSource creates an auto-refreshing OAuth2 token source
// from a Google Cloud service account JSON key file.
// The returned TokenSource automatically refreshes expired tokens.
func newGoogleTokenSource(jsonKeyPath string) (oauth2.TokenSource, error) {
	data, err := os.ReadFile(jsonKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read google api file %q: %w", jsonKeyPath, err)
	}

	creds, err := google.CredentialsFromJSONWithParams(
		context.Background(),
		data,
		google.CredentialsParams{
			Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("parse google credentials: %w", err)
	}

	return creds.TokenSource, nil
}
