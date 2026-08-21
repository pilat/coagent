package lsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	Tags           []int            `json:"tags,omitempty"`
	Deprecated     bool             `json:"deprecated,omitempty"`
	ContainerName  string           `json:"containerName,omitempty"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

type SymbolInformation struct {
	Name          string         `json:"name"`
	Kind          int            `json:"kind"`
	Tags          []int          `json:"tags,omitempty"`
	Deprecated    bool           `json:"deprecated,omitempty"`
	ContainerName string         `json:"containerName,omitempty"`
	Location      SymbolLocation `json:"location"`
}

// SymbolLocation keeps URI-only WorkspaceSymbol locations distinct from a
// Location, whose range is required by the LSP specification.
type SymbolLocation struct {
	URI   string `json:"uri"`
	Range *Range `json:"range,omitempty"`
}

type Diagnostic struct {
	Range    Range          `json:"range"`
	Severity int            `json:"severity,omitempty"`
	Code     DiagnosticCode `json:"code,omitempty"`
	Source   string         `json:"source,omitempty"`
	Message  string         `json:"message"`
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
	Name           string          `json:"name"`
	Kind           int             `json:"kind"`
	Detail         string          `json:"detail,omitempty"`
	URI            string          `json:"uri"`
	Range          Range           `json:"range"`
	SelectionRange Range           `json:"selectionRange"`
	Tags           []int           `json:"tags,omitempty"`
	Data           json.RawMessage `json:"data,omitempty"`
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
	Diagnostics []Diagnostic `json:"diagnostics"`
	version     diagnosticVersion
}

// DiagnosticCode preserves LSP's string-or-integer diagnostic code union.
type DiagnosticCode json.RawMessage

func (c *DiagnosticCode) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		return errors.New("diagnostic code must not be null")
	}

	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		*c = append((*c)[:0], trimmed...)
		return nil
	}

	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()

	if err := decoder.Decode(&number); err != nil {
		return fmt.Errorf("diagnostic code: %w", err)
	}

	if _, err := strconv.ParseInt(number.String(), 10, 64); err != nil {
		return errors.New("diagnostic code must be a string or integer")
	}

	*c = append((*c)[:0], trimmed...)

	return nil
}

func (c DiagnosticCode) MarshalJSON() ([]byte, error) {
	if len(c) == 0 {
		return []byte("null"), nil
	}

	return c, nil
}

func (p *PublishDiagnosticsParams) UnmarshalJSON(data []byte) error {
	var wire struct {
		URI         string          `json:"uri"`
		Version     json.RawMessage `json:"version"`
		Diagnostics []Diagnostic    `json:"diagnostics"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("publish diagnostics: %w", err)
	}

	p.URI = wire.URI
	p.Diagnostics = wire.Diagnostics
	p.version = diagnosticVersion{}

	if len(wire.Version) == 0 {
		return nil
	}

	if bytes.Equal(bytes.TrimSpace(wire.Version), []byte("null")) {
		return errors.New("diagnostic version must not be null")
	}

	var version int
	if err := json.Unmarshal(wire.Version, &version); err != nil {
		return fmt.Errorf("diagnostic version: %w", err)
	}

	p.version = diagnosticVersion{present: true, value: version}

	return nil
}

type diagnosticVersion struct {
	present bool
	value   int
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

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message)
}

type ResponseError = RPCError

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
