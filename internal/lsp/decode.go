package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const markupPlaintext = "plaintext"

//nolint:tagliatelle // LSP defines these camelCase wire keys.
type locationLink struct {
	TargetURI            string `json:"targetUri"`
	TargetRange          *Range `json:"targetRange"`
	TargetSelectionRange *Range `json:"targetSelectionRange"`
}

func decodeLocations(raw json.RawMessage) ([]Location, error) {
	if isJSONNull(raw) {
		return nil, nil
	}

	if len(raw) > 0 && raw[0] == '{' {
		var location Location
		if err := json.Unmarshal(raw, &location); err != nil {
			return nil, fmt.Errorf("location: %w", err)
		}

		return []Location{location}, nil
	}

	var locations []Location
	if err := json.Unmarshal(raw, &locations); err == nil && locationsAreComplete(locations) {
		return locations, nil
	}

	var links []locationLink
	if err := json.Unmarshal(raw, &links); err != nil {
		return nil, fmt.Errorf("location links: %w", err)
	}

	locations = make([]Location, 0, len(links))
	for _, link := range links {
		locations = append(locations, Location{URI: link.TargetURI, Range: link.targetRange()})
	}

	return locations, nil
}

func decodeDocumentSymbols(raw json.RawMessage) ([]DocumentSymbol, error) {
	if isJSONNull(raw) {
		return nil, nil
	}

	var symbols []DocumentSymbol
	if err := json.Unmarshal(
		raw,
		&symbols,
	); err == nil && !documentSymbolsUseLocations(raw) &&
		documentSymbolsAreHierarchical(symbols) {
		return symbols, nil
	}

	var information []SymbolInformation
	if err := json.Unmarshal(raw, &information); err != nil {
		return nil, fmt.Errorf("document symbols: %w", err)
	}

	symbols = make([]DocumentSymbol, 0, len(information))
	for _, item := range information {
		if item.Location.Range == nil {
			return nil, fmt.Errorf("document symbol %q has URI-only location", item.Name)
		}

		symbols = append(symbols, DocumentSymbol{
			Name:           item.Name,
			Kind:           item.Kind,
			Tags:           item.Tags,
			Deprecated:     item.Deprecated,
			ContainerName:  item.ContainerName,
			Range:          *item.Location.Range,
			SelectionRange: *item.Location.Range,
		})
	}

	return symbols, nil
}

func decodeWorkspaceSymbols(raw json.RawMessage) ([]SymbolInformation, error) {
	if isJSONNull(raw) {
		return nil, nil
	}

	var symbols []SymbolInformation
	if err := json.Unmarshal(raw, &symbols); err != nil {
		return nil, fmt.Errorf("workspace symbols: %w", err)
	}

	return symbols, nil
}

func decodeHover(raw json.RawMessage) (*Hover, error) {
	if isJSONNull(raw) {
		return nil, nil //nolint:nilnil // LSP null is a successful no-hover response.
	}

	var wire struct {
		Contents json.RawMessage `json:"contents"`
		Range    *Range          `json:"range"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("hover: %w", err)
	}

	contents, err := decodeHoverContents(wire.Contents)
	if err != nil {
		return nil, err
	}

	return &Hover{Contents: contents, Range: wire.Range}, nil
}

func decodeHoverContents(raw json.RawMessage) (MarkupContent, error) {
	var markup struct {
		Kind  string          `json:"kind"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &markup); err == nil && markup.Kind != "" &&
		len(markup.Value) > 0 && !isJSONNull(markup.Value) {
		var value string
		if err := json.Unmarshal(markup.Value, &value); err == nil {
			return MarkupContent{Kind: markup.Kind, Value: value}, nil
		}
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return MarkupContent{Kind: markupPlaintext, Value: text}, nil
	}

	var marked struct {
		Language string `json:"language"`
		Value    string `json:"value"`
	}
	if err := json.Unmarshal(raw, &marked); err == nil && marked.Value != "" {
		return MarkupContent{Kind: "markdown", Value: fencedMarkdown(marked.Language, marked.Value)}, nil
	}

	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return MarkupContent{}, fmt.Errorf("hover contents: %w", err)
	}

	values := make([]string, 0, len(entries))
	plaintext := true

	for _, entry := range entries {
		content, err := decodeHoverContents(entry)
		if err != nil {
			return MarkupContent{}, err
		}

		values = append(values, content.Value)
		plaintext = plaintext && content.Kind == markupPlaintext
	}

	kind := "markdown"
	if plaintext {
		kind = markupPlaintext
	}

	return MarkupContent{Kind: kind, Value: joinHoverValues(values)}, nil
}

func (link locationLink) targetRange() Range {
	if link.TargetSelectionRange != nil {
		return *link.TargetSelectionRange
	}

	if link.TargetRange != nil {
		return *link.TargetRange
	}

	return Range{}
}

func isJSONNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func locationsAreComplete(locations []Location) bool {
	for _, location := range locations {
		if location.URI == "" {
			return false
		}
	}

	return true
}

func documentSymbolsAreHierarchical(symbols []DocumentSymbol) bool {
	for _, symbol := range symbols {
		if symbol.Name == "" {
			return false
		}
	}

	return true
}

func documentSymbolsUseLocations(raw json.RawMessage) bool {
	var values []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || len(values) == 0 {
		return false
	}

	_, found := values[0]["location"]

	return found
}

func joinHoverValues(values []string) string {
	return string(bytes.Join(func() [][]byte {
		joined := make([][]byte, len(values))
		for i, value := range values {
			joined[i] = []byte(value)
		}

		return joined
	}(), []byte("\n\n")))
}

func fencedMarkdown(language, value string) string {
	if language == "" {
		return "```\n" + value + "\n```"
	}

	return "```" + language + "\n" + value + "\n```"
}
