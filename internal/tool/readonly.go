package tool

// WritableTool declares a tool which can change state outside the session. It
// is spelled out rather than defaulted so that adding a tool forces an explicit
// answer, and so the unsafe direction is never the one you get by forgetting.
type WritableTool struct{}

func (WritableTool) ReadOnly() bool { return false }

// ReadOnlyTool declares a tool which only observes.
type ReadOnlyTool struct{}

func (ReadOnlyTool) ReadOnly() bool { return true }
