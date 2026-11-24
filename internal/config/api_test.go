package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIClients_Validation(t *testing.T) {
	hash := strings.Repeat("a", 64)
	client := fmt.Sprintf("{id: alpha, role: operator, token_sha256: %s}", hash)
	tests := []struct {
		name     string
		clients  string
		managers string
		want     string
	}{
		{name: "operator", clients: client},
		{name: "app", clients: strings.ReplaceAll(client, "operator", "app")},
		{
			name:    "app empty projects",
			clients: strings.ReplaceAll(client, "role: operator", "role: app, projects: []"),
			want:    `api client "alpha": projects must not be empty; omit projects to reach every project`,
		},
		{name: "app projects", clients: strings.ReplaceAll(client, "role: operator", "role: app, projects: [/a, /b]")},
		{
			name: "relative project", clients: strings.ReplaceAll(client, "role: operator", "role: app, projects: [a]"),
			want: `api client "alpha" project "a" must be a clean absolute path`,
		},
		{
			name:    "unclean project",
			clients: strings.ReplaceAll(client, "role: operator", "role: app, projects: [/a/../b]"),
			want:    `api client "alpha" project "/a/../b" must be a clean absolute path`,
		},
		{
			name:    "duplicate project",
			clients: strings.ReplaceAll(client, "role: operator", "role: app, projects: [/a, /a]"),
			want:    `api client "alpha" lists project "/a" twice`,
		},
		{
			name:    "operator projects",
			clients: strings.ReplaceAll(client, "role: operator", "role: operator, projects: [/a]"),
			want:    `api client "alpha": projects apply only to role app`,
		},
		{
			name:    "operator empty projects",
			clients: strings.ReplaceAll(client, "role: operator", "role: operator, projects: []"),
			want:    `api client "alpha": projects apply only to role app`,
		},
		{name: "absent"},
		{name: "missing id", clients: "{role: operator}", want: "api client at index 0 has no id specified"},
		{
			name:    "invalid id",
			clients: "{id: Alpha}",
			want:    `api client id "Alpha" must match ^[a-z0-9][a-z0-9_-]{0,31}$`,
		},
		{name: "long id", clients: "{id: " + strings.Repeat("a", 33) + "}", want: "must match"},
		{name: "duplicate id", clients: client + ", " + client, want: `duplicate api client id "alpha"`},
		{
			name:     "manager collision",
			clients:  client,
			managers: "managers: [{id: alpha, driver: telegram, enabled: false, bot_token: test-token, allowed_user_ids: [1]}]\n",
			want:     `api client id "alpha" is also a manager id`,
		},
		{
			name:    "unsupported role",
			clients: "{id: alpha, role: admin}",
			want:    `api client "alpha" has unsupported role "admin", must be: operator, app`,
		},
		{
			name:    "missing role",
			clients: "{id: alpha}",
			want:    `api client "alpha" has unsupported role "", must be: operator, app`,
		},
		{
			name:    "missing hash",
			clients: "{id: alpha, role: operator}",
			want:    `api client "alpha" token_sha256 must be 64 lowercase hex characters`,
		},
		{
			name:    "uppercase hash",
			clients: strings.ReplaceAll(client, hash, strings.ToUpper(hash)),
			want:    "token_sha256 must be 64 lowercase hex characters",
		},
		{
			name:    "short hash",
			clients: strings.ReplaceAll(client, hash, "abc"),
			want:    "token_sha256 must be 64 lowercase hex characters",
		},
		{
			name:    "nonhex hash",
			clients: strings.ReplaceAll(client, hash, strings.Repeat("z", 64)),
			want:    "token_sha256 must be 64 lowercase hex characters",
		},
		{
			name:    "shared token",
			clients: client + ", " + strings.ReplaceAll(client, "alpha", "beta"),
			want:    `api clients "alpha" and "beta" share a token`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc := tt.managers
			if tt.clients != "" {
				doc += "api:\n  clients: [" + tt.clients + "]\n"
			}
			cfg, err := ParseAndResolve([]byte(doc), nil)
			if tt.want != "" {
				require.ErrorContains(t, err, tt.want)
				return
			}
			require.NoError(t, err)
			if tt.clients != "" {
				require.Len(t, cfg.API.Clients, 1)
				assert.Equal(t, "alpha", cfg.API.Clients[0].ID)
			}
		})
	}
}
