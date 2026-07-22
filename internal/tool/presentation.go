package tool

// Presentation is display-only metadata a tool declares about itself so that
// renderers never branch on tool identity. It is deliberately kept out of
// Definition: Definition is marshalled wholesale into the model's tool guidance
// (see the system context builder), and presentation detail would cost prompt
// tokens on every turn for information the model never uses.
type Presentation struct {
	// Label describes how to summarise an invocation from its input.
	Label LabelSpec `json:"label,omitempty"`
	// Redact names input fields whose values must never be displayed.
	Redact []string `json:"redact,omitempty"`
	// Muted marks an invocation as low-salience, rendered in a dimmer style.
	Muted bool `json:"muted,omitempty"`
	// Result selects how a successful result body is rendered, if at all.
	Result ResultRender `json:"result,omitempty"`
	// Output describes how streamed output is handled. OutputNone folds two
	// concerns deliberately: it suppresses both streamed deltas and error text,
	// because both follow from a tool having nothing displayable to show.
	Output OutputMode `json:"output,omitempty"`
	// Subagent marks a tool whose invocations create child task activity, so a
	// renderer can nest that activity beneath the invoking row.
	Subagent bool `json:"subagent,omitempty"`
	// LabelInPermission asks a permission prompt to show Label rather than the
	// tool's own request description, for tools whose description would echo a
	// redacted value.
	LabelInPermission bool `json:"label_in_permission,omitempty"`
}

// LabelSpec selects a labelling strategy. The zero value selects LabelFields,
// which covers every tool whose label is a projection of its input.
type LabelSpec struct {
	Kind   LabelKind    `json:"kind,omitempty"`
	Fields []LabelField `json:"fields,omitempty"`
	// Source names the input keys the non-default kinds read.
	Source []string `json:"source,omitempty"`
	// Prefix and Noun render LabelItemCount, as "<Prefix> · <n> <Noun>s".
	Prefix string `json:"prefix,omitempty"`
	Noun   string `json:"noun,omitempty"`
}

// LabelField selects one component of a label from the tool's input.
type LabelField struct {
	// Names are candidate input keys in priority order; the first present
	// non-empty string wins.
	Names []string `json:"names,omitempty"`
	// Quote renders the chosen value quoted.
	Quote bool `json:"quote,omitempty"`
	// Default is substituted when no candidate is present.
	Default string `json:"default,omitempty"`
	// Array marks Names as locating an array; Item then selects within its
	// first element, and Overflow appends a count of the remainder.
	Array    bool     `json:"array,omitempty"`
	Item     []string `json:"item,omitempty"`
	Overflow bool     `json:"overflow,omitempty"`
}

// LabelKind names a rendering strategy rather than a tool, so that a tool with
// bespoke label formatting opts into a strategy other tools may also use.
type LabelKind string

const (
	// LabelFields projects Fields from the input. This is the zero value.
	LabelFields LabelKind = ""
	// LabelPatchTargets parses edit targets out of a patch body.
	LabelPatchTargets LabelKind = "patch_targets"
	// LabelItemCount replaces the label with a count of items in a collection.
	LabelItemCount LabelKind = "item_count"
)

// OutputMode describes how a tool's streamed output should be presented.
type OutputMode string

const (
	// OutputBlock accumulates streamed deltas into a result block.
	OutputBlock OutputMode = ""
	// OutputTail replaces the result block with the tail of streamed output,
	// for tools that drive a terminal.
	OutputTail OutputMode = "tail"
	// OutputNone suppresses streamed deltas and error text entirely, for tools
	// whose output would disclose a redacted value.
	OutputNone OutputMode = "none"
)

// ResultRender selects how a successful result body is rendered.
type ResultRender string

const (
	// ResultNone renders no result body.
	ResultNone ResultRender = ""
	// ResultText renders the result as a truncated text block.
	ResultText ResultRender = "text"
	// ResultTodos renders the result as a structured todo list.
	ResultTodos ResultRender = "todos"
)

// BasePresentation supplies the neutral presentation. Tools embed it and
// override Presentation only when a renderer needs more than the tool ID.
type BasePresentation struct{}

// Presentation implements Tool for tools with nothing to declare.
func (BasePresentation) Presentation() Presentation { return Presentation{} }

// SystemPromptGuidance implements Tool for tools that have no extra system
// prompt explanation. Tools override this to inject runtime-behavior guidance.
func (BasePresentation) SystemPromptGuidance() string { return "" }

// PresentationEntry pairs a tool ID with its declared presentation. It is a
// projection parallel to Definition rather than a field of it.
type PresentationEntry struct {
	ID           string       `json:"id"`
	Presentation Presentation `json:"presentation"`
}
