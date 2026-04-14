package lsp

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

type SymbolInformation struct {
	Name     string   `json:"name"`
	Kind     int      `json:"kind"`
	Location Location `json:"location"`
}

type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"`
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type CallHierarchyItem struct {
	Name           string `json:"name"`
	Kind           int    `json:"kind"`
	Detail         string `json:"detail,omitempty"`
	URI            string `json:"uri"`
	Range          Range  `json:"range"`
	SelectionRange Range  `json:"selectionRange"`
}

type CallHierarchyIncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

type CallHierarchyOutgoingCall struct {
	To         CallHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     int          `json:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      int64            `json:"id,omitempty"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError   `json:"error,omitempty"`
}

type ResponseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("LSP error %d: %s", e.Code, e.Message)
}

type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type FileDiagnostics struct {
	Path        string
	Diagnostics []Diagnostic
}

// FormatDiagnostics formats diagnostics for LLM prompt (opencode style).
// Format: ERROR [line:col] message
func FormatDiagnostics(fileDiags []FileDiagnostics) string {
	if len(fileDiags) == 0 {
		return ""
	}

	var sb strings.Builder

	sb.WriteString("LSP errors detected in this file, please fix:\n")

	for _, fd := range fileDiags {
		fmt.Fprintf(&sb, "<diagnostics file=\"%s\">\n", fd.Path)

		for _, d := range fd.Diagnostics {
			fmt.Fprintf(&sb, "ERROR [%d:%d] %s\n",
				d.Range.Start.Line+1,
				d.Range.Start.Character+1,
				d.Message)
		}

		sb.WriteString("</diagnostics>\n")
	}

	return sb.String()
}
