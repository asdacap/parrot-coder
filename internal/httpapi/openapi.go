package httpapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
)

type operationSchema struct {
	request  any
	response any
	status   string
}

var operationSchemas = map[string]operationSchema{
	"getHealth":              {response: v1.Health{}, status: "200"},
	"getRuntime":             {response: v1.Runtime{}, status: "200"},
	"listSessions":           {response: v1.SessionList{}, status: "200"},
	"createSession":          {request: v1.CreateSessionRequest{}, response: v1.Session{}, status: "201"},
	"getSession":             {response: v1.Session{}, status: "200"},
	"deleteSession":          {status: "204"},
	"updateSessionSelection": {request: v1.UpdateSessionSelectionRequest{}, response: v1.SessionSelection{}, status: "200"},
	"listMessages":           {response: v1.MessageList{}, status: "200"},
	"createPrompt":           {request: v1.PromptRequest{}, response: v1.PromptAccepted{}, status: "202"},
	"compactSession":         {response: v1.Compaction{}, status: "202"},
	"interruptSession":       {status: "204"},
	"streamSessionEvents":    {response: v1.Event{}, status: "200"},
	"listPermissions":        {response: v1.PermissionList{}, status: "200"},
	"replyPermission":        {request: v1.PermissionReply{}, status: "204"},
	"listQuestions":          {response: v1.QuestionList{}, status: "200"},
	"replyQuestion":          {request: v1.QuestionReply{}, status: "204"},
	"undo":                   {response: v1.SnapshotTransaction{}, status: "200"},
	"redo":                   {response: v1.SnapshotTransaction{}, status: "200"},
	"listModels":             {response: v1.ModelList{}, status: "200"},
	"listAgents":             {response: v1.AgentList{}, status: "200"},
	"getOpenAPI":             {status: "200"},
}

func buildOpenAPI() []byte {
	definitions := make(map[string]any)
	paths := make(map[string]map[string]any)
	for _, route := range routes {
		spec := operationSchemas[route.OperationID]
		path := paths[route.Path]
		if path == nil {
			path = make(map[string]any)
			paths[route.Path] = path
		}
		responses := map[string]any{
			"400": problemResponse("Invalid request"),
			"404": problemResponse("Not found"),
			"405": problemResponse("Method not allowed"),
			"409": problemResponse("Conflict"),
			"413": problemResponse("Request body too large"),
			"415": problemResponse("Unsupported media type"),
			"500": problemResponse("Internal error"),
		}
		success := map[string]any{"description": "Success"}
		if spec.response != nil {
			mediaType := v1.MediaTypeJSON
			if route.OperationID == "streamSessionEvents" {
				mediaType = v1.MediaTypeSSE
			}
			success["content"] = map[string]any{mediaType: map[string]any{"schema": schemaFor(reflect.TypeOf(spec.response), definitions)}}
		} else if route.OperationID == "getOpenAPI" {
			success["content"] = map[string]any{v1.MediaTypeJSON: map[string]any{"schema": map[string]any{"type": "object"}}}
		}
		responses[spec.status] = success
		operation := map[string]any{"operationId": route.OperationID, "responses": responses}
		if spec.request != nil {
			operation["requestBody"] = map[string]any{"required": true, "content": map[string]any{v1.MediaTypeJSON: map[string]any{"schema": schemaFor(reflect.TypeOf(spec.request), definitions)}}}
		}
		var parameters []any
		for _, name := range pathParameters(route.Path) {
			parameters = append(parameters, map[string]any{"name": name, "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
		}
		if route.OperationID == "streamSessionEvents" {
			parameters = append(parameters, map[string]any{"name": "after", "in": "query", "required": false, "schema": map[string]any{"type": "integer", "format": "int64", "minimum": -1}})
		}
		if route.OperationID == "listSessions" || route.OperationID == "listMessages" {
			parameters = append(parameters,
				map[string]any{"name": "cursor", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				map[string]any{"name": "limit", "in": "query", "required": false, "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}},
			)
		}
		if len(parameters) > 0 {
			operation["parameters"] = parameters
		}
		path[strings.ToLower(route.Method)] = operation
	}
	schemaFor(reflect.TypeOf(v1.Problem{}), definitions)
	for _, payload := range []any{v1.Empty{}, v1.MessagePartDelta{}, v1.SessionStatus{}, v1.PermissionResolved{}, v1.QuestionResolved{}} {
		schemaFor(reflect.TypeOf(payload), definitions)
	}
	document := map[string]any{
		"openapi":          "3.1.0",
		"info":             map[string]any{"title": "Parrot Coder API", "version": "1.0.0"},
		"paths":            paths,
		"components":       map[string]any{"schemas": definitions},
		"x-event-manifest": v1.EventManifest,
	}
	data, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return data
}

func schemaFor(value reflect.Type, definitions map[string]any) any {
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value == reflect.TypeOf(time.Time{}) {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	if value == reflect.TypeOf(json.RawMessage{}) {
		return map[string]any{}
	}
	switch value.Kind() {
	case reflect.Struct:
		name := value.Name()
		if _, exists := definitions[name]; !exists {
			// Install a placeholder first so recursive DTOs remain safe.
			definitions[name] = map[string]any{}
			properties := make(map[string]any)
			var required []string
			for i := 0; i < value.NumField(); i++ {
				field := value.Field(i)
				tag := field.Tag.Get("json")
				fieldName, options, _ := strings.Cut(tag, ",")
				if fieldName == "-" {
					continue
				}
				if fieldName == "" {
					fieldName = field.Name
				}
				properties[fieldName] = schemaFor(field.Type, definitions)
				if !strings.Contains(options, "omitempty") {
					required = append(required, fieldName)
				}
			}
			definition := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
			if len(required) > 0 {
				definition["required"] = required
			}
			definitions[name] = definition
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaFor(value.Elem(), definitions)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(value.Elem(), definitions)}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "string"}
	}
}

func pathParameters(path string) []string {
	var names []string
	for {
		start := strings.IndexByte(path, '{')
		if start < 0 {
			return names
		}
		end := strings.IndexByte(path[start:], '}')
		if end < 0 {
			return names
		}
		names = append(names, path[start+1:start+end])
		path = path[start+end+1:]
	}
}

func problemResponse(description string) map[string]any {
	return map[string]any{"description": description, "content": map[string]any{v1.MediaTypeProblem: map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Problem"}}}}
}
