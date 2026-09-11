package builtin

import (
	"fmt"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/pilat/coagent/internal/safefile"
)

//nolint:wsl_v5 // Fail-open classification keeps each rejection guard next to its evidence.
func directProjectCatTargets(command string, access safefile.Access) ([]string, bool) {
	if access == nil {
		return nil, false
	}

	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) != 1 {
		return nil, false
	}
	stmt := file.Stmts[0]
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || stmt.Semicolon.IsValid() || stmt.Background || stmt.Negated || len(stmt.Redirs) != 0 ||
		len(call.Assigns) != 0 || len(call.Args) < 2 {
		return nil, false
	}

	words := make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		value, static := staticShellWord(word)
		if !static {
			return nil, false
		}
		words = append(words, value)
	}
	if words[0] != "cat" {
		return nil, false
	}

	targets := make([]string, 0, len(words)-1)
	for _, operand := range words[1:] {
		if strings.HasPrefix(operand, "-") {
			return nil, false
		}
		info, path, err := access.Stat(operand)
		if err != nil || !info.Mode().IsRegular() || !withinRoot(access.CanonicalRoot(), path.Canonical) {
			return nil, false
		}
		targets = append(targets, path.Canonical)
	}

	return targets, true
}

func staticShellWord(word *syntax.Word) (string, bool) {
	var b strings.Builder
	if !appendStaticParts(&b, word.Parts) {
		return "", false
	}

	return b.String(), true
}

//nolint:wsl_v5 // Each accepted AST node appends its literal in place.
func appendStaticParts(b *strings.Builder, parts []syntax.WordPart) bool {
	for _, part := range parts {
		switch node := part.(type) {
		case *syntax.Lit:
			b.WriteString(node.Value)
		case *syntax.SglQuoted:
			if node.Dollar {
				return false
			}
			b.WriteString(node.Value)
		case *syntax.DblQuoted:
			if node.Dollar || !appendStaticParts(b, node.Parts) {
				return false
			}
		default:
			return false
		}
	}

	return true
}

func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

//nolint:wsl_v5 // Compact examples are rendered in one pass.
func catPolicyResult(targets []string) string {
	var b strings.Builder
	b.WriteString("Use the read tool for direct project-file reads:\n")
	for _, target := range targets {
		fmt.Fprintf(&b, "read {\"file_path\":%q}\n", target)
	}

	return strings.TrimSpace(b.String())
}
