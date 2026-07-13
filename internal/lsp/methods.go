package lsp

import (
	"context"
	"encoding/json"
	"errors"
)

func (c *Client) DidOpen(ctx context.Context, path, languageID, text string) error {
	c.changeMu.Lock()
	defer c.changeMu.Unlock()
	uri, err := FileURI(c.config.Workspace, path)
	if err != nil {
		return err
	}
	doc := documentState{URI: uri, LanguageID: languageID, Text: text, Version: 1}
	if err := c.Notify(ctx, "textDocument/didOpen", didOpenParams(doc)); err != nil {
		return err
	}
	c.mu.Lock()
	c.docs[uri] = doc
	c.mu.Unlock()
	return nil
}

func (c *Client) DidChange(ctx context.Context, path, text string) error {
	c.changeMu.Lock()
	defer c.changeMu.Unlock()
	uri, err := FileURI(c.config.Workspace, path)
	if err != nil {
		return err
	}
	c.mu.Lock()
	doc, ok := c.docs[uri]
	if ok {
		doc.Version++
		doc.Text = text
	}
	c.mu.Unlock()
	if !ok {
		return errors.New("lsp: document is not open")
	}
	params := map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": doc.Version},
		"contentChanges": []map[string]any{{"text": text}},
	}
	if err := c.Notify(ctx, "textDocument/didChange", params); err != nil {
		return err
	}
	c.mu.Lock()
	c.docs[uri] = doc
	c.mu.Unlock()
	return nil
}

func (c *Client) Definition(ctx context.Context, path string, position Position) ([]Location, error) {
	var raw json.RawMessage
	if err := c.textDocumentCall(ctx, "textDocument/definition", path, position, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var locations []Location
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &locations); err != nil {
			return nil, err
		}
		if len(locations) == 0 || locations[0].URI != "" {
			return locations, nil
		}
		var links []LocationLink
		if err := json.Unmarshal(raw, &links); err != nil {
			return nil, err
		}
		locations = make([]Location, len(links))
		for i, link := range links {
			locations[i] = Location{URI: link.TargetURI, Range: link.TargetSelectionRange}
		}
		return locations, nil
	}
	var location Location
	if err := json.Unmarshal(raw, &location); err != nil {
		return nil, err
	}
	if location.URI == "" {
		var link LocationLink
		if err := json.Unmarshal(raw, &link); err != nil {
			return nil, err
		}
		location = Location{URI: link.TargetURI, Range: link.TargetSelectionRange}
	}
	return []Location{location}, nil
}

func (c *Client) DocumentVersion(path string) (int, bool) {
	uri, err := FileURI(c.config.Workspace, path)
	if err != nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	doc, ok := c.docs[uri]
	return doc.Version, ok
}

func (c *Client) References(ctx context.Context, path string, position Position, includeDeclaration bool) ([]Location, error) {
	uri, err := FileURI(c.config.Workspace, path)
	if err != nil {
		return nil, err
	}
	var locations []Location
	err = c.Call(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": uri}, "position": position,
		"context": map[string]any{"includeDeclaration": includeDeclaration},
	}, &locations)
	return locations, err
}

func (c *Client) Hover(ctx context.Context, path string, position Position) (*Hover, error) {
	var hover *Hover
	err := c.textDocumentCall(ctx, "textDocument/hover", path, position, &hover)
	return hover, err
}

func (c *Client) DocumentSymbols(ctx context.Context, path string) ([]SymbolInformation, error) {
	uri, err := FileURI(c.config.Workspace, path)
	if err != nil {
		return nil, err
	}
	var symbols []SymbolInformation
	err = c.Call(ctx, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": uri}}, &symbols)
	return symbols, err
}

func (c *Client) Symbols(ctx context.Context, query string) ([]SymbolInformation, error) {
	var symbols []SymbolInformation
	err := c.Call(ctx, "workspace/symbol", map[string]any{"query": query}, &symbols)
	return symbols, err
}

func (c *Client) textDocumentCall(ctx context.Context, method, path string, position Position, result any) error {
	uri, err := FileURI(c.config.Workspace, path)
	if err != nil {
		return err
	}
	return c.Call(ctx, method, map[string]any{"textDocument": map[string]any{"uri": uri}, "position": position}, result)
}

func (c *Client) Diagnostics(uri DocumentURI) []Diagnostic {
	if path, err := PathFromURI(c.config.Workspace, uri); err == nil {
		uri, _ = FileURI(c.config.Workspace, path)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Diagnostic(nil), c.diags[uri]...)
}

func (c *Client) AllDiagnostics() map[DocumentURI][]Diagnostic {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[DocumentURI][]Diagnostic, len(c.diags))
	for uri, diagnostics := range c.diags {
		result[uri] = append([]Diagnostic(nil), diagnostics...)
	}
	return result
}
