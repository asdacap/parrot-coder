package event

// Name identifies an event understood by Broker.Publish.
type Name string

const (
	TurnStarted   Name = "turn.started"
	TurnWorking   Name = "turn.working"
	TurnProgress  Name = "turn.progress"
	TurnFinished  Name = "turn.finished"
	TurnCompleted Name = "turn.completed"
	PlanCompleted Name = "plan.completed"
)

func (n Name) Valid() bool {
	switch n {
	case TurnStarted, TurnWorking, TurnProgress, TurnFinished, TurnCompleted, PlanCompleted:
		return true
	default:
		return false
	}
}

// PlanCompletedPayload carries the completed plan and the interaction policy
// declared by the producing mode.
type PlanCompletedPayload struct {
	SessionID string
	MessageID string
	Markdown  string
	Dialog    TurnCompleteDialog
}

// TurnCompleteDialog describes a choice prompt shown after a turn completes.
type TurnCompleteDialog struct {
	Prompt            string
	Context           []string
	Choices           []DialogChoice
	CustomChoice      string
	CustomDescription string
	CustomPrompt      string
	EmptyMessage      string
}

type DialogChoice struct {
	Value       string
	Description string
	Aliases     []string
	Action      ChoiceAction
}

type ChoiceAction struct {
	Agent  string
	Prompt string
}

// BrokerEvent carries one of the predefined broker events and its domain payload.
type BrokerEvent struct {
	Name    Name
	Payload any
}

// EventBroker is the event publication boundary used by runtime domains.
type EventBroker interface {
	Publish(BrokerEvent) func()
}

// NoopBroker accepts events without publishing them.
type NoopBroker struct{}

func (NoopBroker) Publish(BrokerEvent) func() { return func() {} }
