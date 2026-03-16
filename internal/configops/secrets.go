package configops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pilat/coagent/internal/logger"
)

// secretName is what a variable may be called — the same shape the config
// loader's ${VAR} matcher accepts, so a name written here is always resolvable.
var secretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SetSecret edits the file line by line so hand-added comments survive, and
// registers the value for redaction before anything can log it.
func (s *svc) SetSecret(name, value string) (bool, Verdict) {
	if !secretName.MatchString(name) {
		return false, Reject("secrets."+name, errors.New("a secret name must look like AN_ENV_VAR"))
	}

	if value == "" {
		return false, Reject("secrets."+name, errors.New("a secret needs a value"))
	}

	encoded, err := encodeSecretValue(value)
	if err != nil {
		return false, Reject("secrets."+name, err)
	}

	referenced, err := s.referencesSecret(name)
	if err != nil {
		return false, Reject("", err)
	}

	if err := writeSecretLine(s.secretsPath, name, encoded); err != nil {
		return false, Reject("", err)
	}

	logger.AddRedactedValues(value)

	return referenced, OK()
}

// referencesSecret reports whether the raw config already resolves ${name}. A
// referenced variable being rewritten is a rotation, which only takes effect
// after a restart; an unreferenced one is the onboarding case, where the
// reference arrives with the next config write.
func (s *svc) referencesSecret(name string) (bool, error) {
	draft, err := s.rawDraft()
	if err != nil {
		return false, err
	}

	ref := "${" + name + "}"

	for _, p := range draft.Providers {
		if strings.Contains(p.APIKey, ref) {
			return true, nil
		}
	}

	for _, m := range draft.Managers {
		if strings.Contains(m.BotToken, ref) {
			return true, nil
		}
	}

	return false, nil
}

// encodeSecretValue renders a value so the dotenv reader gives it back byte for
// byte. Unquoted values lose a trailing " # …" and expand ${VAR}; single-quoted
// ones are literal, but the format has no escape for a single quote inside them,
// so such a value is refused rather than silently mangled.
func encodeSecretValue(value string) (string, error) {
	if strings.ContainsRune(value, '\'') {
		return "", errors.New("a secret cannot contain a single quote — the secrets file has no way to hold one")
	}

	if strings.ContainsAny(value, "\n\r") {
		return "", errors.New("a secret must be a single line")
	}

	if isPlainSecret(value) {
		return value, nil
	}

	return "'" + value + "'", nil
}

// isPlainSecret reports whether a value survives the unquoted form untouched.
func isPlainSecret(value string) bool {
	for _, r := range value {
		if r <= ' ' || r > '~' || strings.ContainsRune("#$\"\\'", r) {
			return false
		}
	}

	return value != ""
}

// writeSecretLine replaces NAME's line or appends one, leaving every other byte
// of the file alone.
func writeSecretLine(path, name, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read secrets file: %w", err)
	}

	line := name + "=" + value

	out, replaced := replaceAssignment(string(existing), name, line)
	if !replaced {
		out = appendLine(out, line)
	}

	if err := writeFileAtomic(path, []byte(out)); err != nil {
		return fmt.Errorf("write secrets file: %w", err)
	}

	return nil
}

// replaceAssignment rewrites the first assignment of name and drops every later
// one — the reader is last-wins, so a stale duplicate outlives the rotation.
func replaceAssignment(body, name, line string) (string, bool) {
	if body == "" {
		return body, false
	}

	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	replaced := false

	for _, l := range lines {
		switch {
		case !assignsSecret(l, name):
			out = append(out, l)
		case !replaced:
			out = append(out, line)
			replaced = true
		}
	}

	return strings.Join(out, "\n"), replaced
}

// assignsSecret matches every form the dotenv reader accepts: leading space, an
// `export ` prefix, space around the separator, and `:` as well as `=`.
func assignsSecret(line, name string) bool {
	rest := strings.TrimLeft(line, " \t")
	if after, ok := strings.CutPrefix(rest, "export"); ok && strings.IndexFunc(after, isSpaceByte) == 0 {
		rest = strings.TrimLeft(after, " \t")
	}

	rest, ok := strings.CutPrefix(rest, name)
	if !ok {
		return false
	}

	rest = strings.TrimLeft(rest, " \t")

	return strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ":")
}

func isSpaceByte(r rune) bool { return r == ' ' || r == '\t' }

func appendLine(body, line string) string {
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}

	return body + line + "\n"
}

// SecretVarForProvider is the variable a provider's key is stored under. A
// rename never renames the variable: the ${VAR} reference travels with the
// entry, and renaming would orphan the credential.
func SecretVarForProvider(name string) string { return upperSnake(name) + "_API_KEY" }

func secretVarForManager(id string) string { return "MANAGER_" + upperSnake(id) + "_BOT_TOKEN" }

// Ref renders a secret variable as the reference form config accepts.
func Ref(name string) string { return "${" + name + "}" }

// upperSnake turns an entity name into a variable-safe upper-snake form.
func upperSnake(s string) string {
	var b strings.Builder

	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	out := strings.Trim(b.String(), "_")
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = "V" + out
	}

	return out
}
