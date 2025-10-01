package meej

import (
	"fmt"
	"net"
	"strconv"

	"github.com/jfreymuth/pulse/proto"
	"go.uber.org/zap"
)

type paSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	client *proto.Client
	conn   net.Conn

	sessionUpdates chan SessionUpdate
}

func newSessionFinder(logger *zap.SugaredLogger) (SessionFinder, error) {
	client, conn, err := proto.Connect("")
	if err != nil {
		logger.Warnw("Failed to establish PulseAudio connection", "error", err)
		return nil, fmt.Errorf("establish PulseAudio connection: %w", err)
	}

	request := proto.SetClientName{
		Props: proto.PropList{
			"application.name": proto.PropListString("meej"),
		},
	}
	reply := proto.SetClientNameReply{}

	if err := client.Request(&request, &reply); err != nil {
		return nil, err
	}

	sf := &paSessionFinder{
		logger:         logger.Named("session_finder"),
		sessionLogger:  logger.Named("sessions"),
		client:         client,
		conn:           conn,
		sessionUpdates: make(chan SessionUpdate, 1), // buffered channel for notifications
	}

	sf.logger.Debug("Created PA session finder instance")

	// Subscribe to sink input events (session creation/deletion)
	err = client.Request(&proto.Subscribe{Mask: proto.SubscriptionMaskSinkInput}, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("subscribe to PulseAudio sink input events: %w", err)
	}

	newSessionIdx := make(chan uint32, 5)

	go func() {
		for {
			select {
			case idx := <-newSessionIdx:
				var request = proto.GetSinkInputInfo{
					SinkInputIndex: idx,
				}
				var info proto.GetSinkInputInfoReply
				err := client.Request(&request, &info)
				if err != nil {
					sf.logger.Error("Error:", err)
				}

				name, ok := info.Properties["application.process.binary"]
				if !ok {
					sf.logger.Warnw("Failed to get sink input's process name",
						"sinkInputIndex", info.SinkInputIndex)
					continue
				}
				sf.sessionUpdates <- SessionUpdate{Added: newPASession(sf.sessionLogger, sf.client, info.SinkInputIndex, info.Channels, name.String())}
			}
		}
	}()

	client.Callback = func(msg interface{}) {
		switch msg := msg.(type) {
		case *proto.SubscribeEvent:
			if msg.Event&proto.EventFacilityMask == proto.EventSinkSinkInput {
				if msg.Event.GetType() == proto.EventNew {
					newSessionIdx <- msg.Index
				} else if msg.Event.GetType() == proto.EventRemove {
					sf.sessionUpdates <- SessionUpdate{Deleted: strconv.Itoa(int(msg.Index))}
				}
			}
		}
	}

	return sf, nil
}

func (sf *paSessionFinder) GetAllSessions() ([]Session, error) {
	sessions := []Session{}

	// get the master sink session
	masterSink, err := sf.getMasterSinkSession()
	if err == nil {
		sessions = append(sessions, masterSink)
	} else {
		sf.logger.Warnw("Failed to get master audio sink session", "error", err)
	}

	// get the master source session
	masterSource, err := sf.getMasterSourceSession()
	if err == nil {
		sessions = append(sessions, masterSource)
	} else {
		sf.logger.Warnw("Failed to get master audio source session", "error", err)
	}

	// enumerate sink inputs and add sessions along the way
	if err := sf.enumerateAndAddSessions(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate audio sessions", "error", err)
		return nil, fmt.Errorf("enumerate audio sessions: %w", err)
	}

	return sessions, nil
}

func (sf *paSessionFinder) Release() error {
	if err := sf.conn.Close(); err != nil {
		sf.logger.Warnw("Failed to close PulseAudio connection", "error", err)
		return fmt.Errorf("close PulseAudio connection: %w", err)
	}

	sf.logger.Debug("Released PA session finder instance")

	return nil
}

func (sf *paSessionFinder) getMasterSinkSession() (Session, error) {
	request := proto.GetSinkInfo{
		SinkIndex: proto.Undefined,
	}
	reply := proto.GetSinkInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master sink info", "error", err)
		return nil, fmt.Errorf("get master sink info: %w", err)
	}

	// create the master sink session
	sink := newMasterSession(sf.sessionLogger, sf.client, reply.SinkIndex, reply.Channels, true)

	return sink, nil
}

func (sf *paSessionFinder) getMasterSourceSession() (Session, error) {
	request := proto.GetSourceInfo{
		SourceIndex: proto.Undefined,
	}
	reply := proto.GetSourceInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master source info", "error", err)
		return nil, fmt.Errorf("get master source info: %w", err)
	}

	// create the master source session
	source := newMasterSession(sf.sessionLogger, sf.client, reply.SourceIndex, reply.Channels, false)

	return source, nil
}

func (sf *paSessionFinder) enumerateAndAddSessions(sessions *[]Session) error {
	request := proto.GetSinkInputInfoList{}
	reply := proto.GetSinkInputInfoListReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get sink input list", "error", err)
		return fmt.Errorf("get sink input list: %w", err)
	}

	for _, info := range reply {
		name, ok := info.Properties["application.process.binary"]

		if !ok {
			sf.logger.Warnw("Failed to get sink input's process name",
				"sinkInputIndex", info.SinkInputIndex)

			continue
		}

		// create the meej session object
		newSession := newPASession(sf.sessionLogger, sf.client, info.SinkInputIndex, info.Channels, name.String())

		// add it to our slice
		*sessions = append(*sessions, newSession)

	}

	return nil
}

func (sf *paSessionFinder) SessionUpdates() <-chan SessionUpdate {
	return sf.sessionUpdates
}
