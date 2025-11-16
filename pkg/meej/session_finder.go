package meej

type SessionUpdate struct {
	Added         Session
	Deleted       string
	IOAdded       Session
	MasterChanged NewMaster
}

type NewMaster struct {
	endpointID string
	flowDir    bool // true=input, false=output
}

// SessionFinder represents an entity that can find all current audio sessions
type SessionFinder interface {
	GetAllSessions() ([]Session, error)

	SessionUpdates() <-chan SessionUpdate

	Release() error
}
