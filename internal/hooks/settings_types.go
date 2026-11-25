package hooks

type handler struct {
	command     string
	args        []string
	execForm    bool
	timeout     float64
	async       bool
	onFailure   string
	matcherText string
	match       matcher
}

// Input-only lowercase tags retain camelCase matching under the local JSON tag rule.
type settingsFile struct {
	Path            string                    `json:"-"`
	Hooks           map[string][]matcherGroup `json:"hooks"`
	DisableAllHooks *bool                     `json:"disableallhooks"`
}

type matcherGroup struct {
	Matcher string          `json:"matcher"`
	Hooks   []handlerConfig `json:"hooks"`
}

type handlerConfig struct {
	Type        string    `json:"type"`
	Command     string    `json:"command"`
	Args        *[]string `json:"args"`
	Timeout     float64   `json:"timeout"`
	Shell       string    `json:"shell"`
	If          *string   `json:"if"`
	Async       bool      `json:"async"`
	AsyncRewake bool      `json:"asyncrewake"`
	OnFailure   string    `json:"onfailure"`
}

type handlerKey struct {
	event   string
	matcher string
	command string
	args    string
}
