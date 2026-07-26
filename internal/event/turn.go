package event

// Name identifies an event understood by Broker.Publish.
type Name string

const (
	TurnStarted   Name = "turn.started"
	TurnWorking   Name = "turn.working"
	TurnProgress  Name = "turn.progress"
	TurnFinished  Name = "turn.finished"
	TurnCompleted Name = "turn.completed"
)

func (n Name) Valid() bool {
	switch n {
	case TurnStarted, TurnWorking, TurnProgress, TurnFinished, TurnCompleted:
		return true
	default:
		return false
	}
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
