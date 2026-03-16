package configops

import "strings"

// Verdict is the synchronous answer to every mutation. A rejection is a
// successful call with Applied=false, and persists nothing.
type Verdict struct {
	Applied  bool         `json:"applied"`
	Errors   []FieldError `json:"errors,omitempty"`
	Warnings []string     `json:"warnings,omitempty"`
}

// FieldError points at the config path that caused a rejection, so a caller can
// anchor the message at the field rather than dumping it at the top.
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// OK is the verdict for a change that applied cleanly.
func OK() Verdict {
	return Verdict{Applied: true}
}

// OKWith is an applied change that had a non-blocking problem — a manager that
// failed to start, say. The change is live; the warning says what to watch.
func OKWith(warnings ...string) Verdict {
	return Verdict{Applied: true, Warnings: warnings}
}

// Reject builds a rejection anchored at path. An empty path means the problem is
// the config as a whole.
func Reject(path string, err error) Verdict {
	return Verdict{Errors: []FieldError{{Path: path, Message: err.Error()}}}
}

// Failed reports whether the verdict rejected the change.
func (v Verdict) Failed() bool { return !v.Applied }

// Reason renders the verdict's errors as one line, for CLI exit paths and tool
// results. It is deliberately not named Error: a verdict is a result, not an
// error value, and a rejection is a successful call.
func (v Verdict) Reason() string {
	if len(v.Errors) == 0 {
		return ""
	}

	var b strings.Builder

	for i, e := range v.Errors {
		if i > 0 {
			b.WriteString("; ")
		}

		if e.Path != "" {
			b.WriteString(e.Path + ": ")
		}

		b.WriteString(e.Message)
	}

	return b.String()
}
