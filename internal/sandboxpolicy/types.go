package sandboxpolicy

// Mode is a rule's access mode.
type Mode string

const (
	ModeReadOnly  Mode = "ro"
	ModeReadWrite Mode = "rw"
)

// Action is what a rule does to a path.
type Action string

const (
	ActionAllow Action = "allow"
	ActionDeny  Action = "deny"
)

// ObjectKind is the filesystem kind found at a rule's anchor.
type ObjectKind string

const (
	KindDir    ObjectKind = "dir"
	KindFile   ObjectKind = "file"
	KindSocket ObjectKind = "socket"
)

// Rule is one operator authority statement. Exactly one of Allow or Deny is set.
type Rule struct {
	Allow string `json:"allow,omitempty" yaml:"allow,omitempty"`
	Deny  string `json:"deny,omitempty"  yaml:"deny,omitempty"`
	Mode  Mode   `json:"mode,omitempty"  yaml:"mode,omitempty"`
}

// ProjectRules are the rules that apply to one configured project.
type ProjectRules struct {
	Rules []Rule `json:"rules,omitempty" yaml:"rules,omitempty"`
}

// Section is the operator-supplied sandbox configuration.
type Section struct {
	Enabled  bool                    `json:"enabled"            yaml:"enabled"`
	Rules    []Rule                  `json:"rules,omitempty"    yaml:"rules,omitempty"`
	Projects map[string]ProjectRules `json:"projects,omitempty" yaml:"projects,omitempty"`
}

func (k ObjectKind) resolved() ObjectKind {
	if k == KindFile || k == KindSocket {
		return k
	}

	return KindDir
}
