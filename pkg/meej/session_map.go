package meej

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MixyLabs/meej/pkg/meej/util"
	"github.com/thoas/go-funk"
	"go.uber.org/zap"
)

type sessionMapper struct {
	meej   *Meej
	logger *zap.SugaredLogger

	mapping          map[string][]Session // (slider idx or special selector) to sessions
	ioSessions       []Session
	masterInSession  Session
	masterOutSession Session
	lock             sync.Locker

	sessionFinder SessionFinder

	lastAudioFlyoutShown time.Time
}

const (
	masterSessionName = "master" // master device volume
	systemSessionName = "system" // system sounds volume
	inputSessionName  = "mic"    // microphone input level

	// some targets need to be transformed before their correct audio sessions can be accessed.
	// this prefix identifies those targets to ensure they don't contradict with another similarly-named process
	specialTargetTransformPrefix = "meej."

	// targets the currently active window (Windows-only)
	specialTargetCurrentWindow = "current"

	// targets all currently unmapped sessions
	specialTargetAllUnmapped = "unmapped"
)

// this matches friendly device names (on Windows), e.g. "Headphones (Realtek Audio)"
var deviceSessionKeyPattern = regexp.MustCompile(`^.+ \(.+\)$`)

func newSessionMapper(meej *Meej, logger *zap.SugaredLogger, sessionFinder SessionFinder) (*sessionMapper, error) {
	logger = logger.Named("sessions")

	m := &sessionMapper{
		meej:          meej,
		logger:        logger,
		mapping:       make(map[string][]Session),
		lock:          &sync.Mutex{},
		sessionFinder: sessionFinder,
	}

	logger.Debug("Created session map instance")

	return m, nil
}

func (m *sessionMapper) initialize() error {
	m.setupOnSessionsChanged() // will receive IO sessions during getAndAddSessions

	if err := m.getAndAddSessions(); err != nil {
		m.logger.Warnw("Failed to get all sessions during session map initialization", "error", err)
		return fmt.Errorf("get all sessions during init: %w", err)
	}

	m.setupOnConfigReload()
	m.setupOnSliderMove()

	return nil
}

func (m *sessionMapper) release() error {
	if err := m.sessionFinder.Release(); err != nil {
		m.logger.Warnw("Failed to release session finder during session map release", "error", err)
		return fmt.Errorf("release session finder during release: %w", err)
	}

	return nil
}

// assumes the session map is clean!
// only call on a new session map or as part of refreshSessions which calls reset
func (m *sessionMapper) getAndAddSessions() error {
	//mapping.lastSessionRefresh = time.Now()

	sessions, err := m.sessionFinder.GetAllSessions()
	if err != nil {
		m.logger.Warnw("Failed to get sessions from session finder", "error", err)
		return fmt.Errorf("get sessions from SessionFinder: %w", err)
	}

	for _, session := range sessions {
		m.addSession(session)
	}

	m.logger.Infow("Got all audio sessions successfully", "sessionMapper", m)

	return nil
}

func (m *sessionMapper) setupOnConfigReload() {
	configReloadedChannel := m.meej.configMan.SubscribeToChanges()

	go func() {
		for {
			select {
			case <-configReloadedChannel:
				m.logger.Info("Detected config reload, attempting to re-acquire all audio sessions")
				m.clear()
				if err := m.getAndAddSessions(); err != nil {
					m.logger.Warnw("Failed to re-acquire audio sessions after config reload", "error", err)
				}
			}
		}
	}()
}

func (m *sessionMapper) setupOnSliderMove() {
	sliderEventsChannel := m.meej.mixy.SubscribeToSliderMoveEvents()

	go func() {
		for {
			select {
			case event := <-sliderEventsChannel:
				m.handleSliderMoveEvent(event)
			}
		}
	}()
}

func (m *sessionMapper) setupOnSessionsChanged() {
	sessionUpdatesChannel := m.sessionFinder.SessionUpdates()

	go func() {
		for {
			select {
			case update := <-sessionUpdatesChannel:
				if update.Added != nil {
					m.addSession(update.Added)
					m.logger.Debugw("Session added", "session", update.Added.Key())
				}
				if update.Deleted != "" {
					if m.removeSession(update.Deleted) {
						m.logger.Debugw("Session removed", "sessionKey", update.Deleted)
					} else {
						m.logger.Warnw("Session to remove not found in map", "sessionKey", update.Deleted)
					}
				}
				if update.IOAdded != nil {
					sessionPresent := false
					for _, session := range m.ioSessions {
						if session.InternalKey() == update.IOAdded.InternalKey() {
							sessionPresent = true
						}
					}
					if sessionPresent {
						m.logger.Warnw("Duplicate IO session", "sessionKey", update.IOAdded.Key())
					} else {
						m.ioSessions = append(m.ioSessions, update.IOAdded)
						m.logger.Debugw("IO Session added", "sessionKey", update.IOAdded.Key())
					}
				}
				if update.IORemoved != "" {
					sessionRemoved := false

					// IOAdded ensures no duplicates, so finish on first found
					for i, ioSession := range m.ioSessions {
						if ioSession.InternalKey() == update.IORemoved {
							m.ioSessions = append(m.ioSessions[:i], m.ioSessions[i+1:]...)
							m.logger.Debugw("IO Session removed", "sessionKey", update.IORemoved)
							sessionRemoved = true
							break
						}
					}
					if !sessionRemoved {
						// NOTE: it's super frequent (at least on Windows)
						//m.logger.Warnw("no IO Session to remove", "sessionKey", update.IORemoved)
					}
				}
				if update.MasterChanged != (NewMaster{}) {
					m.setMaster(update.MasterChanged)
				}
			}
		}
	}()
}

func (m *sessionMapper) currProcessSetVolume(value float32) {
	currentWindowProcessNames, err := util.GetCurrentWindowProcessNames()
	if err != nil {
		return
	}

	for targetIdx, target := range currentWindowProcessNames {
		currentWindowProcessNames[targetIdx] = strings.ToLower(target)
	}

	// remove dupes
	currentWindowProcessNames = funk.UniqString(currentWindowProcessNames)

	// FIXME not sure how efficient this is
	for _, sessions := range m.mapping {
		for _, session := range sessions {
			for _, procName := range currentWindowProcessNames {
				if matchSessionToTarget(session, procName) {
					if err := session.SetVolume(value); err != nil {
						m.logger.Warnw("Failed to set volume for session of focused window",
							"currentWindowProcessName", procName, "session", session, "error", err)
					}
					break
				}
			}
		}
	}
}

func (m *sessionMapper) handleSliderMoveEvent(event SliderMoveEvent) {
	sliderTargets, ok := m.meej.currConf().SliderMapping.get(event.SliderID)
	if !ok || len(sliderTargets) == 0 {
		return
	}

	setMasterOutVolume := false
	setMasterInVolume := false
	setCurrProcVolume := false
	setUnmappedSessionVolume := false

	for _, target := range sliderTargets {
		if target == masterSessionName {
			setMasterOutVolume = true
		}
		if target == inputSessionName {
			setMasterInVolume = true
		}
		if target == specialTargetTransformPrefix+specialTargetCurrentWindow {
			setCurrProcVolume = true
		}
		if target == specialTargetTransformPrefix+specialTargetAllUnmapped {
			setUnmappedSessionVolume = true
		}
	}

	if setCurrProcVolume {
		m.currProcessSetVolume(event.PercentValue)
	}

	sessions, _ := m.mapping[strconv.Itoa(event.SliderID)]

	if setMasterOutVolume {
		if m.masterOutSession != nil {
			sessions = append(sessions, m.masterOutSession)

			m.maybeTriggerAudioFlyout()
		} else {
			m.logger.Warn("Master output session is nil, cannot set its volume")
		}
	}
	if setMasterInVolume {
		if m.masterInSession != nil {
			sessions = append(sessions, m.masterInSession)
		} else {
			m.logger.Warn("Master output session is nil, cannot set its volume")
		}
	}

	if setUnmappedSessionVolume {
		unmapped, unmappedOk := m.mapping["-1"]
		if unmappedOk {
			sessions = append(sessions, unmapped...)
		}
	}
	if len(sessions) == 0 {
		return
	}

	for _, session := range sessions {
		//if session.GetVolume() != event.PercentValue {
		if err := session.SetVolume(event.PercentValue); err != nil {
			m.logger.Warnw("Failed to set target session volume", "error", err)
			//adjustmentFailed = true
		}
		//}
	}

}

func (m *sessionMapper) maybeTriggerAudioFlyout() {
	if !m.meej.currConf().AudioFlyout {
		return
	}

	now := time.Now()
	if m.lastAudioFlyoutShown.Add(time.Second).Before(now) {
		m.logger.Debugw("Showing audio flyout for master volume change")
		err := ShowAudioFlyout()
		if err != nil {
			m.logger.Warnw("Cannot display audio flyout: ", err)
		}
		m.lastAudioFlyoutShown = now
	}
}

func (m *sessionMapper) targetHasSpecialTransform(target string) bool {
	return strings.HasPrefix(target, specialTargetTransformPrefix)
}

func matchSessionToTarget(session Session, target string) bool {
	if strings.EqualFold(session.Key(), target) {
		return true
	}

	// also match device sessions by their friendly names (Windows)
	if deviceSessionKeyPattern.MatchString(session.Key()) && strings.EqualFold(session.Key(), target) {
		return true
	}

	return false
}

func (m *sessionMapper) addSession(sess Session) {
	m.lock.Lock()
	defer m.lock.Unlock()

	mappingKey := sess.Key()
	mapped := false

	m.meej.currConf().SliderMapping.iterate(func(sliderIdx int, targets []string) {
		for _, target := range targets {
			if matchSessionToTarget(sess, target) {
				m.mapping[strconv.Itoa(sliderIdx)] = append(m.mapping[strconv.Itoa(sliderIdx)], sess)
				mapped = true
			}
		}
	})

	// leave special or device sessions unmapped, they need to be mapped explicitly
	if !mapped {
		if !deviceSessionKeyPattern.MatchString(mappingKey) &&
			!funk.ContainsString([]string{masterSessionName, systemSessionName, inputSessionName}, mappingKey) {
			m.mapping["-1"] = append(m.mapping["-1"], sess)
		} else {
			// every session needs to live to avoid crashes
			m.mapping["_"] = append(m.mapping["_"], sess)
		}
	}
}

func (m *sessionMapper) removeSession(sessionKey string) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	anyRemoved := false

	for key, sessions := range m.mapping {
		newSessions := sessions[:0]
		for _, session := range sessions {
			if session.InternalKey() != sessionKey {
				newSessions = append(newSessions, session)
			} else {
				//don't session.Release(), its already dead
				anyRemoved = true
			}
		}
		m.mapping[key] = newSessions
	}

	return anyRemoved
}

func (m *sessionMapper) setMaster(newMaster NewMaster) {
	m.lock.Lock()
	defer m.lock.Unlock()

	for _, ioSession := range m.ioSessions {
		if ioSession.InternalKey() == newMaster.endpointID {
			if newMaster.flowDir {
				m.masterInSession = ioSession
			} else {
				m.masterOutSession = ioSession
			}
			m.logger.Infow("Set new master session", "session", ioSession, "flowDir", newMaster.flowDir)
			return
		}
	}
}

func (m *sessionMapper) get(key string) ([]Session, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	value, ok := m.mapping[key]
	return value, ok
}

func (m *sessionMapper) clear() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.logger.Debug("Releasing and clearing all audio sessions")

	for key, sessions := range m.mapping {
		for _, session := range sessions {
			session.Release()
		}

		delete(m.mapping, key)
	}

	m.masterOutSession = nil
	m.masterInSession = nil

	// IO sessions got released above
	m.ioSessions = nil

	m.logger.Debug("Session map cleared")
}

func (m *sessionMapper) String() string {
	m.lock.Lock()
	defer m.lock.Unlock()

	sessionCount := 0

	for _, value := range m.mapping {
		sessionCount += len(value)
	}

	return fmt.Sprintf("<%d audio sessions>", sessionCount)
}
