package meej

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"github.com/diegosz/go-wca/pkg/wca"
	"github.com/go-ole/go-ole"
	"go.uber.org/zap"
)

type ManagerWithNotif struct {
	manager      *wca.IAudioSessionManager2
	notification *wca.IAudioSessionNotification
}

type wcaSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	eventCtx *ole.GUID // needed for some session actions to successfully notify other audio consumers

	// needed for device change notifications
	mmDeviceEnumerator      *wca.IMMDeviceEnumerator
	mmNotificationClient    *wca.IMMNotificationClient
	lastDefaultDeviceChange time.Time

	sessionNotifications []ManagerWithNotif

	stop           chan struct{}
	releaseSession chan CallbackRegistration

	sessionUpdates chan SessionUpdate
}

const (
	randomGUID = "{1ec920a1-7db8-44ba-9779-e5d28ed9f330}"

	// the notification client will call this multiple times in quick succession based on the
	// default device's assigned media roles, so we need to filter out the extraneous calls
	//minDefaultDeviceChangeThreshold = 100 * time.Millisecond

	// prefix for device sessions in logger
	deviceSessionFormat = "device.%s"
)

type CallbackRegistration struct {
	session        *wca.IAudioSessionControl
	nativeCallback *wca.IAudioSessionEvents
}

func (sf *wcaSessionFinder) setupSessionCallback(release chan CallbackRegistration, session *wca.IAudioSessionControl,
	sessionName string) (*wca.IAudioSessionEvents, error) {
	var releaseFunc func()

	callback := wca.IAudioSessionEventsCallback{
		OnStateChanged: func(newState wca.AudioSessionState) error {
			return sf.onStateChanged(sessionName, releaseFunc, newState)
		},
		OnSessionDisconnected: func(disconnectReason wca.AudioSessionDisconnectReason) error {
			return sf.onSessionDisconnected(sessionName, releaseFunc, disconnectReason)
		},
	}
	ase := wca.NewIAudioSessionEvents(callback)
	releaseFunc = func() {
		release <- CallbackRegistration{session, ase}
	}
	err := session.RegisterAudioSessionNotification(ase)
	if err != nil {
		// ase should be Released
		return nil, fmt.Errorf("Error registering audio session notification: %v\n", err)
	}

	return ase, err
}

func (sf *wcaSessionFinder) onStateChanged(sessionName string, release func(), newState wca.AudioSessionState) error {
	//fmt.Printf("called OnStateChanged for %s to %d\n", sessionName, newState)

	if newState == wca.AudioSessionStateExpired {
		select {
		case sf.sessionUpdates <- SessionUpdate{Deleted: sessionName}:
		default:
			panic("Cannot send to chan")
		}
		release()
	}

	return nil
}

func (sf *wcaSessionFinder) onSessionDisconnected(sessionName string, release func(), disconnectReason wca.AudioSessionDisconnectReason) error {
	fmt.Printf("called OnSessionDisconnected %q\t%d\n", sessionName, disconnectReason)
	release()

	select {
	case sf.sessionUpdates <- SessionUpdate{Deleted: sessionName}:
	default:
		panic("Cannot send to chan")
	}

	return nil
}

func (sf *wcaSessionFinder) onSessionCreated(pNewSession *wca.IAudioSessionControl) error {
	newSessionObj, err := sf.processNewSession(pNewSession)
	if err != nil {
		sf.logger.Errorw("Failed to process new session from OnSessionCreated", "error", err)
		// don't return the error, otherwise the callback will fail, and we won't get any more notifications
		return nil
	}

	//fmt.Printf("called OnSessionCreated for %s\n", newSessionObj.name)

	select {
	case sf.sessionUpdates <- SessionUpdate{Added: newSessionObj}:
	default:
		panic("Cannot send to chan")
	}

	return nil
}

func newSessionFinder(logger *zap.SugaredLogger) (SessionFinder, error) {
	sf := &wcaSessionFinder{
		logger:         logger.Named("session_finder"),
		sessionLogger:  logger.Named("sessions"),
		eventCtx:       ole.NewGUID(randomGUID),
		sessionUpdates: make(chan SessionUpdate, 5), // buffered channel for notifications
		releaseSession: make(chan CallbackRegistration, 1),
		stop:           make(chan struct{}),
	}

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		// E_FALSE means that the call was redundant.
		const eFalse = 1
		oleError := &ole.OleError{}

		if errors.As(err, &oleError) {
			if oleError.Code() == eFalse {
				sf.logger.Warn("CoInitializeEx failed with E_FALSE due to redundant invocation")
			} else {
				sf.logger.Warnw("Failed to call CoInitializeEx",
					"isOleError", true,
					"error", err,
					"oleError", oleError)

				return nil, fmt.Errorf("call CoInitializeEx: %w", err)
			}
		} else {
			sf.logger.Warnw("Failed to call CoInitializeEx",
				"isOleError", false,
				"error", err,
				"oleError", nil)

			return nil, fmt.Errorf("call CoInitializeEx: %w", err)
		}

	}

	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator,
		0,
		wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator,
		&sf.mmDeviceEnumerator,
	); err != nil {
		sf.logger.Warnw("Failed to call CoCreateInstance", "error", err)
		return nil, fmt.Errorf("call CoCreateInstance: %w", err)
	}

	go func() {
		for {
			select {
			case registration := <-sf.releaseSession:
				// registration.nativeCallback is Released inside
				if err := registration.session.UnregisterAudioSessionNotification(registration.nativeCallback); err != nil {
					fmt.Printf("Error unregistering audio session notification: %v\n", err)
				}
				registration.session.Release()
			case <-sf.stop:
				return
			}
		}
	}()

	sf.logger.Debug("Created WCA session finder instance")
	return sf, nil
}

func (sf *wcaSessionFinder) SessionUpdates() <-chan SessionUpdate {
	return sf.sessionUpdates
}

func (sf *wcaSessionFinder) GetAllSessions() ([]Session, error) {
	var sessions []Session

	// hopefully trigger GC
	for _, notif := range sf.sessionNotifications {
		if notif.manager != nil && notif.notification != nil {
			_ = notif.manager.UnregisterSessionNotification(notif.notification)
		}
	}
	sf.sessionNotifications = nil

	// receive notifications whenever the default device changes (only do this once)
	if sf.mmNotificationClient == nil {
		if err := sf.registerDefaultDeviceChangeCallback(); err != nil {
			sf.logger.Warnw("Failed to register default device change callback", "error", err)
			return nil, fmt.Errorf("register default device change callback: %w", err)
		}
	}

	// enumerate all devices and make their "master" sessions bindable by friendly name;
	// for output devices, this is also where we enumerate process sessions
	if err := sf.enumerateAndAddSessions(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate device sessions", "error", err)
		return nil, fmt.Errorf("enumerate device sessions: %w", err)
	}

	// enumerateAndAddSessions already registered IO sessions
	// now just get the currently active default output and input devices
	// and send MasterChanged updates for them
	defaultOutputEndpoint, defaultInputEndpoint, err := sf.getDefaultAudioEndpoints()
	if err != nil {
		sf.logger.Warnw("Failed to get default audio endpoints", "error", err)
		return nil, fmt.Errorf("get default audio endpoints: %w", err)
	}
	defer defaultOutputEndpoint.Release()
	if defaultInputEndpoint != nil {
		defer defaultInputEndpoint.Release()
	}

	var endpointID string
	if err := defaultOutputEndpoint.GetId(&endpointID); err != nil {
		sf.logger.Warnw("Failed to get endpointID of default output device", "error", err)
		return nil, fmt.Errorf("get default output endpointID: %w", err)
	}
	select {
	case sf.sessionUpdates <- SessionUpdate{MasterChanged: NewMaster{endpointID, false}}:
	default:
		panic("Cannot send to chan")
	}

	if defaultInputEndpoint != nil {
		if err := defaultInputEndpoint.GetId(&endpointID); err != nil {
			sf.logger.Warnw("Failed to get endpointID of default input device", "error", err)
			return nil, fmt.Errorf("get default input endpointID: %w", err)
		}
		select {
		case sf.sessionUpdates <- SessionUpdate{MasterChanged: NewMaster{endpointID, true}}:
		default:
			panic("Cannot send to chan")
		}
	}

	return sessions, nil
}

func (sf *wcaSessionFinder) Release() error {
	if sf.mmNotificationClient != nil {
		_ = sf.mmDeviceEnumerator.UnregisterEndpointNotificationCallback(sf.mmNotificationClient)
	}

	if sf.mmDeviceEnumerator != nil {
		sf.mmDeviceEnumerator.Release()
	}

	ole.CoUninitialize()

	close(sf.stop)

	sf.logger.Debug("Released WCA session finder instance")
	return nil
}

func (sf *wcaSessionFinder) getDefaultAudioEndpoints() (*wca.IMMDevice, *wca.IMMDevice, error) {
	// get the default audio endpoints as IMMDevice instances
	var mmOutDevice *wca.IMMDevice
	var mmInDevice *wca.IMMDevice

	if err := sf.mmDeviceEnumerator.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &mmOutDevice); err != nil {
		sf.logger.Warnw("Failed to call GetDefaultAudioEndpoint (out)", "error", err)
		return nil, nil, fmt.Errorf("call GetDefaultAudioEndpoint (out): %w", err)
	}

	// allow this call to fail (not all users have a microphone connected)
	if err := sf.mmDeviceEnumerator.GetDefaultAudioEndpoint(wca.ECapture, wca.EConsole, &mmInDevice); err != nil {
		sf.logger.Warn("No default input device detected, proceeding without it (\"mic\" will not work)")
		mmInDevice = nil
	}

	return mmOutDevice, mmInDevice, nil
}

func (sf *wcaSessionFinder) registerDefaultDeviceChangeCallback() error {
	callback := wca.IMMNotificationClientCallback{
		OnDeviceAdded:          sf.deviceAddedCallback,
		OnDeviceRemoved:        sf.deviceRemovedCallback,
		OnDeviceStateChanged:   sf.deviceStateChangedCallback,
		OnDefaultDeviceChanged: sf.defaultDeviceChangedCallback,
	}

	sf.mmNotificationClient = wca.NewIMMNotificationClient(callback)

	if err := sf.mmDeviceEnumerator.RegisterEndpointNotificationCallback(sf.mmNotificationClient); err != nil {
		sf.logger.Warnw("Failed to call RegisterEndpointNotificationCallback", "error", err)
		return fmt.Errorf("call RegisterEndpointNotificationCallback: %w", err)
	}

	return nil
}

func (sf *wcaSessionFinder) getMasterSession(mmDevice *wca.IMMDevice, key string, loggerKey string) (*masterSession, error) {
	var audioEndpointVolume *wca.IAudioEndpointVolume

	if err := mmDevice.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &audioEndpointVolume); err != nil {
		sf.logger.Warnw("Failed to activate AudioEndpointVolume for master session", "error", err)
		return nil, fmt.Errorf("activate master session: %w", err)
	}

	var endpointID string
	err := mmDevice.GetId(&endpointID)
	if err != nil {
		sf.logger.Warnw("Failed to get endpointID of master session", "error", err)
		return nil, fmt.Errorf("get master endpointID: %w", err)
	}

	// create the master session
	master, err := newMasterSession(sf.sessionLogger, audioEndpointVolume, sf.eventCtx, key, endpointID, loggerKey)
	if err != nil {
		sf.logger.Warnw("Failed to create master session instance", "error", err)
		return nil, fmt.Errorf("create master session: %w", err)
	}

	return master, nil
}

func (sf *wcaSessionFinder) enumerateAndAddSessions(sessions *[]Session) error {
	// get list of devices
	var deviceCollection *wca.IMMDeviceCollection

	if err := sf.mmDeviceEnumerator.EnumAudioEndpoints(wca.EAll, wca.DEVICE_STATE_ACTIVE, &deviceCollection); err != nil {
		sf.logger.Warnw("Failed to enumerate active audio endpoints", "error", err)
		return fmt.Errorf("enumerate active audio endpoints: %w", err)
	}

	// check how many devices there are
	var deviceCount uint32

	if err := deviceCollection.GetCount(&deviceCount); err != nil {
		sf.logger.Warnw("Failed to get device count from device collection", "error", err)
		return fmt.Errorf("get device count from device collection: %w", err)
	}

	// for each device:
	for deviceIdx := uint32(0); deviceIdx < deviceCount; deviceIdx++ {
		err := func() error {
			// get its IMMDevice instance
			var endpoint *wca.IMMDevice

			if err := deviceCollection.Item(deviceIdx, &endpoint); err != nil {
				sf.logger.Warnw("Failed to get device from device collection",
					"deviceIdx", deviceIdx,
					"error", err)

				return fmt.Errorf("get device %d from device collection: %w", deviceIdx, err)
			}
			defer endpoint.Release()

			// get its IMMEndpoint instance to figure out if it's an output device (and we need to enumerate its process sessions later)
			dispatch, err := endpoint.QueryInterface(wca.IID_IMMEndpoint)
			if err != nil {
				sf.logger.Warnw("Failed to query IMMEndpoint for device",
					"deviceIdx", deviceIdx,
					"error", err)

				return fmt.Errorf("query device %d IMMEndpoint: %w", deviceIdx, err)
			}

			endpointDescription, endpointFriendlyName, err := sf.getEndpointNames(endpoint)
			if err != nil {
				return err
			}

			// receive a useful object instead of our dispatch
			endpointType := (*wca.IMMEndpoint)(dispatch) //unsafe.Pointer
			defer endpointType.Release()

			var dataFlow uint32
			if err := endpointType.GetDataFlow(&dataFlow); err != nil {
				sf.logger.Warnw("Failed to get data flow for endpoint",
					"deviceIdx", deviceIdx,
					"error", err)

				return fmt.Errorf("get device %d data flow: %w", deviceIdx, err)
			}

			sf.logger.Debugw("Enumerated device info",
				"deviceIdx", deviceIdx,
				"deviceDescription", endpointDescription,
				"deviceFriendlyName", endpointFriendlyName,
				"dataFlow", dataFlow)

			// if the device is an output device, enumerate and add its per-process audio sessions
			if dataFlow == wca.ERender {
				if err := sf.enumerateAndAddProcessSessions(endpoint, endpointFriendlyName, sessions); err != nil {
					sf.logger.Warnw("Failed to enumerate and add process sessions for device",
						"deviceIdx", deviceIdx,
						"error", err)

					return fmt.Errorf("enumerate and add device %d process sessions: %w", deviceIdx, err)
				}
			}

			// for all devices (both input and output), add a named "master" session that can be addressed
			// by using the device's friendly name (as appears when the user left-clicks the speaker icon in the tray)
			newIOSession, err := sf.getMasterSession(endpoint,
				endpointFriendlyName,
				fmt.Sprintf(deviceSessionFormat, endpointDescription))

			if err != nil {
				sf.logger.Warnw("Failed to get master session for device",
					"deviceIdx", deviceIdx,
					"error", err)

				return fmt.Errorf("get device %d master session: %w", deviceIdx, err)
			}

			select {
			case sf.sessionUpdates <- SessionUpdate{IOAdded: newIOSession}:
			default:
				panic("Cannot send to chan")
			}

			// add it to our slice
			*sessions = append(*sessions, newIOSession)

			return nil
		}()
		if err != nil {
			return err
		}
	}

	return nil
}

func (sf *wcaSessionFinder) getEndpointNames(endpoint *wca.IMMDevice) (string, string, error) {
	// get the device's property store
	var propertyStore *wca.IPropertyStore

	if err := endpoint.OpenPropertyStore(wca.STGM_READ, &propertyStore); err != nil {
		sf.logger.Warnw("Failed to open property store for endpoint",
			"error", err)

		return "", "", fmt.Errorf("open endpoint property store: %w", err)
	}
	defer propertyStore.Release()

	// query the property store for the device's description and friendly name
	value := &wca.PROPVARIANT{}

	if err := propertyStore.GetValue(&wca.PKEY_Device_DeviceDesc, value); err != nil {
		sf.logger.Warnw("Failed to get description for device",
			"error", err)

		return "", "", fmt.Errorf("get device description: %w", err)
	}

	// device description i.e. "Headphones"
	endpointDescription := strings.ToLower(value.String())

	if err := propertyStore.GetValue(&wca.PKEY_Device_FriendlyName, value); err != nil {
		sf.logger.Warnw("Failed to get friendly name for device",
			"error", err)

		return "", "", fmt.Errorf("get device friendly name: %w", err)
	}

	// device friendly name i.e. "Headphones (Realtek Audio)"
	endpointFriendlyName := value.String()
	return endpointDescription, endpointFriendlyName, nil
}

func (sf *wcaSessionFinder) enumerateAndAddProcessSessions(
	endpoint *wca.IMMDevice,
	endpointFriendlyName string,
	sessions *[]Session,
) error {
	sf.logger.Debugw("Enumerating and adding process sessions for audio output device",
		"deviceFriendlyName", endpointFriendlyName)

	var audioSessionManager2 *wca.IAudioSessionManager2

	if err := endpoint.Activate(
		wca.IID_IAudioSessionManager2,
		wca.CLSCTX_ALL,
		nil,
		&audioSessionManager2,
	); err != nil {
		sf.logger.Warnw("Failed to activate endpoint as IAudioSessionManager2", "error", err)
		return fmt.Errorf("activate endpoint: %w", err)
	}

	// TODO: also register for new audio endpoints in deviceAddedCallback
	callback := wca.IAudioSessionNotificationCallback{
		OnSessionCreated: func(pNewSession *wca.IAudioSessionControl) error {
			return sf.onSessionCreated(pNewSession)
		},
	}
	asn := wca.NewIAudioSessionNotification(callback)
	if err := audioSessionManager2.RegisterSessionNotification(asn); err != nil {
		return err
	}

	// keep references so it doesn't get GC'd
	sf.sessionNotifications = append(sf.sessionNotifications, ManagerWithNotif{audioSessionManager2, asn})

	var sessionEnumerator *wca.IAudioSessionEnumerator

	if err := audioSessionManager2.GetSessionEnumerator(&sessionEnumerator); err != nil {
		return err
	}
	defer sessionEnumerator.Release()

	var sessionCount int
	if err := sessionEnumerator.GetCount(&sessionCount); err != nil {
		sf.logger.Warnw("Failed to get session count from session enumerator", "error", err)
		return fmt.Errorf("get session count: %w", err)
	}

	sf.logger.Debugw("Got session count from session enumerator", "count", sessionCount)

	// for each session:
	for sessionIdx := 0; sessionIdx < sessionCount; sessionIdx++ {
		// get the IAudioSessionControl
		var audioSessionControl *wca.IAudioSessionControl
		if err := sessionEnumerator.GetSession(sessionIdx, &audioSessionControl); err != nil {
			sf.logger.Warnw("Failed to get session from session enumerator",
				"error", err,
				"sessionIdx", sessionIdx)

			return fmt.Errorf("get session %d from enumerator: %w", sessionIdx, err)
		}

		newSession, err := sf.processNewSession(audioSessionControl)
		if err != nil {
			sf.logger.Warnw("Failed to process a new session", "error", err, "sessionIdx", sessionIdx)
			continue
		}

		*sessions = append(*sessions, newSession)
	}

	return nil
}

func (sf *wcaSessionFinder) processNewSession(audioSessionControl *wca.IAudioSessionControl) (*wcaSession, error) {
	audioSessionControl.AddRef()

	// query its IAudioSessionControl2
	dispatch, err := audioSessionControl.QueryInterface(wca.IID_IAudioSessionControl2)
	if err != nil {
		sf.logger.Warnw("Failed to query session's IAudioSessionControl2",
			"error", err)

		audioSessionControl.Release()
		return nil, err
	}

	// receive a useful object instead of our dispatch
	audioSessionControl2 := (*wca.IAudioSessionControl2)(unsafe.Pointer(dispatch))

	var pid uint32

	// get the session's PID
	if err := audioSessionControl2.GetProcessId(&pid); err != nil {
		// if this is the system sounds session, GetProcessId will error with an undocumented
		// AUDCLNT_S_NO_CURRENT_PROCESS (0x889000D) - this is fine, we actually want to treat it a bit differently
		// The first part of this condition will be true if the call to IsSystemSoundsSession fails
		// The second part will be true if the original error mesage from GetProcessId doesn't contain this magical
		// error code (in decimal format).
		isSystemSoundsErr := audioSessionControl2.IsSystemSoundsSession()
		if isSystemSoundsErr != nil && !strings.Contains(err.Error(), "143196173") {
			// of course, if it's not the system sounds session, we got a problem
			sf.logger.Warnw("Failed to query session's pid",
				"error", err,
				"isSystemSoundsError", isSystemSoundsErr)

			audioSessionControl.Release()
			audioSessionControl2.Release()
			return nil, err
		}

		// update 2020/08/31: this is also the exact case for UWP applications, so we should no longer override the PID.
		// it will successfully update whenever we call GetProcessId for e.g. Video.UI.exe, despite the error being non-nil.
	}

	// get its ISimpleAudioVolume
	dispatch, err = audioSessionControl2.QueryInterface(wca.IID_ISimpleAudioVolume)
	if err != nil {
		sf.logger.Warnw("Failed to query session's ISimpleAudioVolume",
			"error", err)

		audioSessionControl.Release()
		audioSessionControl2.Release()
		return nil, err
	}

	// make it useful, again
	simpleAudioVolume := (*wca.ISimpleAudioVolume)(dispatch) //unsafe.Pointer

	// create the meej session object
	newSession, err := newWCASession(sf.sessionLogger, audioSessionControl2, simpleAudioVolume, pid, sf.eventCtx)

	if err != nil {
		// this could just mean this process is already closed by now, and the session will be cleaned up later by the OS
		if !errors.Is(err, errNoSuchProcess) {
			sf.logger.Warnw("Failed to create new WCA session instance",
				"error", err)

			return nil, fmt.Errorf("create wca session for session: %w", err)
		}

		// in this case, log it and release the session's handles, then skip to the next one
		sf.logger.Debugw("Process already exited, skipping session and releasing handles", "pid", pid)

		audioSessionControl.Release()
		audioSessionControl2.Release()
		simpleAudioVolume.Release()

		return nil, err
	}

	ase, err := sf.setupSessionCallback(sf.releaseSession, audioSessionControl, newSession.name)
	if err != nil {
		return nil, err
	}

	// keep alive
	newSession.ase = ase

	return newSession, nil
}

func (sf *wcaSessionFinder) defaultDeviceChangedCallback(dataflow wca.EDataFlow, role wca.ERole, identifier string) error {
	if role == 2 { // ignore eCommunications
		return nil
	}

	// filter out calls that happen in rapid succession
	now := time.Now()
	if sf.lastDefaultDeviceChange.Add(time.Millisecond * 100).After(now) {
		return nil
	}
	sf.lastDefaultDeviceChange = now

	sf.logger.Debug("Default audio device changed, new id: " + identifier)

	select {
	case sf.sessionUpdates <- SessionUpdate{MasterChanged: NewMaster{identifier, dataflow == 1}}:
	default:
		panic("Cannot send to chan")
	}

	return nil
}

func (sf *wcaSessionFinder) deviceAddedCallback(pwstrDeviceId string) error {
	var endpoint *wca.IMMDevice
	err := sf.mmDeviceEnumerator.GetDevice(pwstrDeviceId, &endpoint)
	if err != nil {
		sf.logger.Warnw("Failed to get MM device for new device", "error", err)
		return nil
	}

	endpointDescription, endpointFriendlyName, err := sf.getEndpointNames(endpoint)

	newIOSession, err := sf.getMasterSession(endpoint,
		endpointFriendlyName,
		fmt.Sprintf(deviceSessionFormat, endpointDescription))

	select {
	case sf.sessionUpdates <- SessionUpdate{IOAdded: newIOSession}:
	default:
		panic("Cannot send to chan")
	}
	return nil
}

func (sf *wcaSessionFinder) deviceRemovedCallback(pwstrDeviceId string) error {

	select {
	case sf.sessionUpdates <- SessionUpdate{IORemoved: pwstrDeviceId}:
	default:
		panic("Cannot send to chan")
	}

	return nil
}
func (sf *wcaSessionFinder) deviceStateChangedCallback(pwstrDeviceId string, dwNewState uint32) error {

	switch dwNewState {
	case wca.DEVICE_STATE_ACTIVE:
		_ = sf.deviceAddedCallback(pwstrDeviceId)
	case wca.DEVICE_STATE_DISABLED, wca.DEVICE_STATE_NOTPRESENT, wca.DEVICE_STATE_UNPLUGGED:
		_ = sf.deviceRemovedCallback(pwstrDeviceId)
	}

	return nil
}
