package meej

type SessionUpdate struct {
	Added   Session
	Deleted string
}

// SessionFinder represents an entity that can find all current audio sessions
type SessionFinder interface {
	GetAllSessions() ([]Session, error)

	SessionUpdates() <-chan SessionUpdate

	Release() error
}
