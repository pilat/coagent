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

func TestAPIListen_Validation(t *testing.T) {
	accepted := []string{"127.0.0.1:7878", "127.0.0.2:7878", "[::1]:7878"}
	refused := []string{
		"0.0.0.0:7878", "[::]:7878", "192.168.1.5:7878", "localhost:7878", ":7878",
		"127.0.0.1", "127.0.0.1:0", "[0:0:0:0:0:0:0:1]:7878", "[::1%lo]:7878",
		"[::ffff:127.0.0.1]:7878",
	}
	for _, listen := range accepted {
		t.Run(listen, func(t *testing.T) {
			cfg := apiListenConfig()
			cfg.API.Listen = listen
			require.NoError(t, cfg.validateAPI())
		})
	}
	for _, listen := range refused {
		t.Run(listen, func(t *testing.T) {
			cfg := apiListenConfig()
			cfg.API.Listen = listen
			require.EqualError(t, cfg.validateAPI(), fmt.Sprintf(
				"api.listen %q must be a loopback IP address and port, such as 127.0.0.1:7878", listen,
			))
		})
	}
}

func TestAPIListen_NeedsAClient(t *testing.T) {
	cfg := &UnifiedConfig{API: APIConfig{Listen: "127.0.0.1:7878"}}
	require.EqualError(t, cfg.validateAPI(), "api.listen requires at least one api client")
}

func TestAPIOrigins_Validation(t *testing.T) {
	accepted := []string{"http://localhost:5173", "http://127.0.0.1:8080", "https://app.example", "http://[::1]:3000"}
	refused := []string{
		"null", "http://localhost:5173/", "http://localhost:5173/app", "HTTP://localhost:5173",
		"http://LOCALHOST:5173", "http://localhost:80", "https://app.example:443", "ftp://host",
		"http://user@localhost:5173", "http://", "http://localhost:5173?x=1", "http://localhost:5173#f",
		"http://localhost:0", "http://localhost:65536", "http://localhost:05173", "http://localhost:",
		"http://localhost?", "http://localhost/#", "http://localhost#",
	}
	for _, origin := range accepted {
		t.Run(origin, func(t *testing.T) {
			cfg := apiListenConfig()
			cfg.API.Origins = []string{origin}
			require.NoError(t, cfg.validateAPI())
		})
	}
	for _, origin := range refused {
		t.Run(origin, func(t *testing.T) {
			cfg := apiListenConfig()
			cfg.API.Origins = []string{origin}
			require.EqualError(t, cfg.validateAPI(), fmt.Sprintf(
				"api origin %q must be scheme://host[:port] with scheme http or https, "+
					"in lowercase, without a default port, path, query or credentials", origin,
			))
		})
	}
	for _, origin := range []string{"*", "http://*", "https://*.example.com"} {
		t.Run(origin, func(t *testing.T) {
			cfg := apiListenConfig()
			cfg.API.Origins = []string{origin}
			require.EqualError(t, cfg.validateAPI(), fmt.Sprintf(
				"api origin %q must be an exact origin; wildcards are not supported", origin,
			))
		})
	}
}

func TestAPIOrigins_ListedTwice(t *testing.T) {
	cfg := apiListenConfig()
	cfg.API.Origins = []string{"http://localhost:5173", "http://localhost:5173"}
	require.EqualError(t, cfg.validateAPI(), `api origin "http://localhost:5173" is listed twice`)
}

func TestAPIOrigins_NeedListen(t *testing.T) {
	cfg := apiListenConfig()
	cfg.API.Listen = ""
	cfg.API.Origins = []string{"http://localhost:5173"}
	require.EqualError(t, cfg.validateAPI(), "api.origins requires api.listen")
}

func apiListenConfig() *UnifiedConfig {
	return &UnifiedConfig{API: APIConfig{
		Listen:  "127.0.0.1:7878",
		Clients: []APIClientEntry{{ID: "alpha", Role: APIRoleOperator, TokenSHA256: strings.Repeat("a", 64)}},
	}}
}
