package meej

import (
	"context"
	"encoding/binary"
	"time"

	"go.uber.org/zap"

	"tinygo.org/x/bluetooth"

	"github.com/MixyLabs/meej/pkg/meej/util"
)

// BLEIO provides a meej-aware abstraction layer to managing mixy I/O
type BLEIO struct {
	comPort  string
	baudRate uint

	meej   *Meej
	logger *zap.SugaredLogger

	devFoundChannel chan bluetooth.ScanResult
	device          *bluetooth.Device
	midiChar        *bluetooth.DeviceCharacteristic
	lastDeviceAddr  string

	stopChannel chan bool
	connected   bool

	currentSliderPercentValues []float32

	sliderMoveConsumers []chan SliderMoveEvent
}

// SliderMoveEvent represents a single slider move captured by meej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

func NewBLEIO(meej *Meej, logger *zap.SugaredLogger) (*BLEIO, error) {
	logger = logger.Named("mixy")

	sio := &BLEIO{
		meej:                       meej,
		logger:                     logger,
		stopChannel:                make(chan bool),
		devFoundChannel:            make(chan bluetooth.ScanResult, 1),
		connected:                  false,
		sliderMoveConsumers:        []chan SliderMoveEvent{},
		currentSliderPercentValues: make([]float32, 6),
	}

	logger.Debug("Created BLE i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

var (
	midiServiceRealUUID    = bluetooth.NewUUID([16]byte{0x03, 0xB8, 0x0E, 0x5A, 0xED, 0xE8, 0x4B, 0x33, 0xA7, 0x51, 0x6C, 0xE3, 0x4E, 0xC4, 0xC7, 0x00})
	midiServiceFakeUUID    = bluetooth.NewUUID([16]byte{0x03, 0xB8, 0x0E, 0x5A, 0xED, 0xE8, 0x4B, 0x33, 0xA7, 0x51, 0x6C, 0xE3, 0x4E, 0xC4, 0xC7, 0x05})
	midiCharacteristicUUID = bluetooth.NewUUID([16]byte{0x77, 0x72, 0xE5, 0xDB, 0x38, 0x68, 0x41, 0x12, 0xA1, 0xA9, 0xF2, 0x66, 0x9D, 0x10, 0x6B, 0xF3})
)

func (bio *BLEIO) Start() error {
	var adapter = bluetooth.DefaultAdapter

	runScan := make(chan struct{})

	err := adapter.Enable()
	if err != nil {
		return err
	}

	// status handler is only used for restarting scans
	// so use only if not using fast reconnect
	if !bio.meej.currConf().FastReconnect {
		adapter.SetConnectHandler(func(device bluetooth.Device, connected bool) {
			if !connected && bio.connected {
				bio.close()
				bio.logger.Infof("Device %s disconnected, restarting scan", device.Address)

				select {
				case runScan <- struct{}{}:
				default:
					panic("Cannot send to chan")
				}
			}
		})
	}

	// fast reconnect mode:
	// it's hard to quickly detect disconnects (at least on Windows)
	// so we will just scan continuously
	// and reconnect whenever original device is found again
	// which currently means it dropped its side of connection
	// normal mode:
	// scan only when not connected
	go func() {
		lastSent := time.Time{}
		for {
			select {
			case <-runScan:
				bio.logger.Debug("Started a scan")
				_ = adapter.Scan(func(adapter *bluetooth.Adapter, result bluetooth.ScanResult) {
					for _, uuid := range result.ServiceUUIDs() {
						if uuid == midiServiceRealUUID {
							now := time.Now()
							if now.Sub(lastSent) >= 5*time.Second {
								bio.logger.Debugf("found MIDI BLE device: %s %s RSSI=%d ", result.LocalName(), result.Address, result.RSSI)
								if !bio.meej.currConf().FastReconnect {
									_ = adapter.StopScan()
								}
								lastSent = now
								select {
								case bio.devFoundChannel <- result:
								default:
									panic("Cannot send to chan")
								}
							}
						}
					}
				})
			}
		}
	}()

	// handle a candidate device
	// after connection, go back to waiting
	// for fresh device to replace current with
	go func() {
		for {
			select {
			case result := <-bio.devFoundChannel:
				if result.Address.String() != bio.lastDeviceAddr && bio.lastDeviceAddr != "" {
					break // reconnect only with the same device
				}

				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				done := make(chan error, 1)

				go func() {
					device, err := adapter.Connect(result.Address, bluetooth.ConnectionParams{})
					if err != nil {
						done <- err
					} else {
						done <- bio.Activate(device)
					}
				}()

				select {
				case <-ctx.Done():
					bio.logger.Error("Connect timed out after 10s")
					if !bio.meej.currConf().FastReconnect {
						runScan <- struct{}{}
					}
				case err := <-done:
					if err != nil {
						bio.logger.Error("Connect error:", err)
						if !bio.meej.currConf().FastReconnect {
							runScan <- struct{}{}
						}
					}
				}

				cancel()
			}
		}
	}()

	go func() {
		for {
			select {
			case <-bio.stopChannel:
				bio.close()
			}
		}
	}()

	// start initial scan
	runScan <- struct{}{}

	return nil
}

func (bio *BLEIO) Activate(device bluetooth.Device) error {
	bio.close() // try to free previous connection

	bio.logger.Debugf("Connected with %s", device.Address)

	services, err := device.DiscoverServices([]bluetooth.UUID{midiServiceRealUUID, midiServiceFakeUUID})
	if err != nil {
		bio.logger.Error("discover MIDI service error:", err.Error())
		return err
	}
	if len(services) == 0 {
		bio.logger.Error("MIDI BLE service not found")
		return err
	}
	midiService := services[0]

	chars, err := midiService.DiscoverCharacteristics([]bluetooth.UUID{midiCharacteristicUUID})
	if err != nil {
		bio.logger.Error("discover MIDI characteristic error:", err.Error())
		return err
	}

	if len(chars) == 0 {
		bio.logger.Error("MIDI BLE characteristic not found")
		return err
	}
	midiChar := chars[0]

	bio.logger.Debug("subscribing to MIDI characteristic notifications...")
	err = midiChar.EnableNotifications(func(buf []byte) {
		ccs := MIDIParseCC(buf)
		if len(ccs) == 0 {
			bio.logger.Debug("(no CC messages)")
			return
		}
		/*for _, cc := range ccs {
			bio.logger.Debug("CC ctrl:", cc[0], "val:", cc[1])
		}*/

		bio.handleCC(ccs)
	})
	if err != nil {
		bio.logger.Error("enable notifications error:", err.Error())
		return err
	}

	_, err = midiChar.Read(make([]byte, 10))
	if err != nil {
		bio.logger.Error("read error:", err.Error())
	}

	bio.connected = true
	bio.device = &device
	bio.midiChar = &midiChar
	bio.lastDeviceAddr = device.Address.String()

	bio.logger.Info("MIDI connection established")

	return nil
}

func (bio *BLEIO) Stop() {
	if bio.connected {
		bio.logger.Debug("Shutting down mixy connection")
		bio.stopChannel <- true
	} else {
		bio.logger.Debug("Not connected, nothing to stop")
	}
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (bio *BLEIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	bio.sliderMoveConsumers = append(bio.sliderMoveConsumers, ch)

	return ch
}

func (bio *BLEIO) setupOnConfigReload() {
	configReloadedChannel := bio.meej.configMan.SubscribeToChanges()

	go func() {
		for {
			select {
			case <-configReloadedChannel:
				err := bio.applyMixyParams()
				if err != nil {
					bio.logger.Error("Failed to apply Mixy params error:", err.Error())
				}
			}
		}
	}()
}

func (bio *BLEIO) applyMixyParams() error {
	if !bio.connected {
		return nil
	}

	params := bio.meej.currConf().MixyParams

	buffer := make([]byte, 8)
	binary.LittleEndian.PutUint16(buffer[0:2], params.ChangeThreshold)
	binary.LittleEndian.PutUint16(buffer[2:4], params.SlowInterval)
	binary.LittleEndian.PutUint16(buffer[4:6], params.FastInterval)
	binary.LittleEndian.PutUint16(buffer[6:8], params.FastTimeout)

	_, err := bio.midiChar.WriteWithoutResponse(buffer)

	return err
}

func (bio *BLEIO) close() {
	if bio.device == nil {
		return
	}

	bio.connected = false

	_ = bio.device.Disconnect()

	bio.device = nil
	bio.midiChar = nil

	bio.logger.Debug("BLE connection closed")
}

type CCMsg [2]int

type CCMessages []CCMsg

func MIDIParseCC(packet []byte) CCMessages {
	var result CCMessages
	i := 0

	// skip header
	if i >= len(packet) || (packet[i]&0x80) == 0 {
		return result // invalid packet
	}
	i++

	for i < len(packet) {
		b := packet[i]

		// Skip any non-timestamp bytes
		if (b & 0x80) == 0 {
			i++
			continue
		}

		// Timestamp byte
		i++
		if i+2 >= len(packet) {
			break // incomplete message
		}

		status := packet[i]
		ctrl := packet[i+1]
		val := packet[i+2]
		i += 3

		// Only parse CC messages
		if (status & 0xF0) == 0xB0 {
			result = append(result, CCMsg{int(ctrl), int(val)})
		}
	}

	return result
}

func (bio *BLEIO) handleCC(ccs CCMessages) {
	var moveEvents []SliderMoveEvent
	for _, cc := range ccs {
		sliderIdx, value := cc[0], cc[1]

		// TODO validate against values based on device model
		if sliderIdx > 5 && value > 127 {
			bio.logger.Debugw("Got unexpected CCMsg, ignoring", "CCMsg", cc)
			return
		}

		normalizedScalar := util.NormalizeScalar(float32(value) / 127.0)

		if bio.meej.currConf().InvertSliders {
			normalizedScalar = 1 - normalizedScalar
		}

		moveEvents = append(moveEvents, SliderMoveEvent{
			SliderID:     sliderIdx,
			PercentValue: normalizedScalar,
		})

		if bio.meej.Verbose() {
			bio.logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
		}
	}

	if len(moveEvents) > 0 {
		for _, consumer := range bio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				consumer <- moveEvent
			}
		}
	}
}
