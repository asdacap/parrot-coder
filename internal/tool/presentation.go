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
	// SuccessIcon replaces the generic icon on a successful invocation.
	SuccessIcon string `json:"success_icon,omitempty"`
	// Result selects how a successful result body is rendered, if at all.
	Result ResultRender `json:"result,omitempty"`
	// ResultCountNoun appends the number of non-empty result lines to the
	// completed label, using this value as the singular noun.
	ResultCountNoun string `json:"result_count_noun,omitempty"`
	// CompletedLabel selects how a successful result enriches the invocation
	// label. The zero value leaves the label unchanged.
	CompletedLabel CompletedLabelKind `json:"completed_label,omitempty"`
	// Output describes how streamed output is handled. OutputNone folds two
	// concerns deliberately: it suppresses both streamed deltas and error text,
	// because both follow from a tool having nothing displayable to show.
	Output OutputMode `json:"output,omitempty"`
	// Failure selects how a failed invocation's error is rendered.
	Failure FailureRender `json:"failure,omitempty"`
	// Subagent marks a tool whose invocations create child task activity, so a
	// renderer can nest that activity beneath the invoking row.
	Subagent bool `json:"subagent,omitempty"`
	// Modeline moves a running invocation in the top-level session from the
	// activity rows into the transient modeline status.
	Modeline bool `json:"modeline,omitempty"`
	// LiveOnly keeps an invocation in the redrawable live surface while active
	// and discards its terminal report instead of committing it to scrollback.
	LiveOnly bool `json:"live_only,omitempty"`
	// LabelInPermission asks a permission prompt to show Label rather than the
	// tool's own request description, for tools whose description would echo a
	// redacted value.
	LabelInPermission bool `json:"label_in_permission,omitempty"`
	// CompletedInput renders selected input fields as a permanent block only
	// after the invocation reaches a terminal state. TerminalOnly also suppresses
	// the transient pending/running row.
	CompletedInput CompletedInputSpec `json:"completed_input,omitempty"`
}

func (p Presentation) clone() Presentation {
	p.Redact = append([]string(nil), p.Redact...)
	p.CompletedInput.Fields = append([]string(nil), p.CompletedInput.Fields...)
	p.Label.Source = append([]string(nil), p.Label.Source...)
	p.Label.Fields = append([]LabelField(nil), p.Label.Fields...)
	for i := range p.Label.Fields {
		p.Label.Fields[i].Names = append([]string(nil), p.Label.Fields[i].Names...)
		p.Label.Fields[i].Item = append([]string(nil), p.Label.Fields[i].Item...)
	}
	return p
}

// CompletedInputSpec describes an input block retained in the transcript.
type CompletedInputSpec struct {
	Fields       []string `json:"fields,omitempty"`
	TerminalOnly bool     `json:"terminal_only,omitempty"`
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
	// TaskName resolves a session or process ID to its human-facing name when one is known.
	TaskName bool `json:"task_name,omitempty"`
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

// CompletedLabelKind names a result-to-label strategy rather than a tool.
type CompletedLabelKind string

const (
	// CompletedLabelNone does not add result details to the label.
	CompletedLabelNone CompletedLabelKind = ""
	// CompletedLabelAnswers appends human-facing answers resolved from the
	// invocation's questions and the structured result.
	CompletedLabelAnswers CompletedLabelKind = "answers"
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

// FailureRender selects how a failed invocation's error is rendered.
type FailureRender string

const (
	// FailureRequest retains the generic failed-request block and status detail.
	FailureRequest FailureRender = ""
	// FailureErrorBlock renders the full error in a dedicated permanent block.
	FailureErrorBlock FailureRender = "error_block"
)

// ResultRender selects how a successful result body is rendered.
type ResultRender string

const (
	// ResultNone renders no result body.
	ResultNone ResultRender = ""
	// ResultText renders the result as a truncated text block.
	ResultText ResultRender = "text"
	// ResultDiff renders a unified diff as a terminal-aware diff block.
	ResultDiff ResultRender = "diff"
	// ResultTodos renders the result as a structured todo list.
	ResultTodos ResultRender = "todos"
)

// BasePresentation supplies the neutral presentation. Tools embed it and
// override Presentation only when a renderer needs more than the tool ID.
type BasePresentation struct{}

// Presentation implements Tool for tools with nothing to declare.
func (BasePresentation) Presentation() Presentation { return Presentation{} }

// PresentationEntry pairs a tool ID with its declared presentation. It is a
// projection parallel to Definition rather than a field of it.
type PresentationEntry struct {
	ID           string       `json:"id"`
	Presentation Presentation `json:"presentation"`
}
