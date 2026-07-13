package lsp

import "encoding/json"

type DocumentURI string

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   DocumentURI `json:"uri"`
	Range Range       `json:"range"`
}

type LocationLink struct {
	OriginSelectionRange *Range      `json:"originSelectionRange,omitempty"`
	TargetURI            DocumentURI `json:"targetUri"`
	TargetRange          Range       `json:"targetRange"`
	TargetSelectionRange Range       `json:"targetSelectionRange"`
}

type Diagnostic struct {
	Range    Range           `json:"range"`
	Severity int             `json:"severity,omitempty"`
	Code     any             `json:"code,omitempty"`
	Source   string          `json:"source,omitempty"`
	Message  string          `json:"message"`
	Data     json.RawMessage `json:"data,omitempty"`
}

type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type Hover struct {
	Contents json.RawMessage `json:"contents"`
	Range    *Range          `json:"range,omitempty"`
}

type SymbolInformation struct {
	Name           string              `json:"name"`
	Kind           int                 `json:"kind"`
	Tags           []int               `json:"tags,omitempty"`
	ContainerName  string              `json:"containerName,omitempty"`
	Location       Location            `json:"location"`
	Range          *Range              `json:"range,omitempty"`
	SelectionRange *Range              `json:"selectionRange,omitempty"`
	Children       []SymbolInformation `json:"children,omitempty"`
}

type ResponseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *ResponseError) Error() string { return "lsp: " + e.Message }

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}
