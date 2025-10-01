// Package meej provides a machine-side client that pairs with an Arduino
// chip to form a tactile, physical volume control system/
package meej

import (
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/MixyLabs/meej/pkg/meej/util"
)

// Meej is the main entity managing all subcomponents
type Meej struct {
	logger    *zap.SugaredLogger
	notifier  Notifier
	configMan *ConfigManager
	mixy      *BLEIO
	sessions  *sessionMapper

	runningWithTray bool
	stopChannel     chan bool
	version         string
	verbose         bool
}

func NewMeej(logger *zap.SugaredLogger, verbose bool) (*Meej, error) {
	logger = logger.Named("meej")

	notifier, err := NewToastNotifier(logger)
	if err != nil {
		logger.Errorw("Failed to create ToastNotifier", "error", err)
		return nil, fmt.Errorf("create new ToastNotifier: %w", err)
	}

	config, err := NewConfig(logger, notifier)
	if err != nil {
		logger.Errorw("Failed to create Config", "error", err)
		return nil, fmt.Errorf("create new Config: %w", err)
	}

	d := &Meej{
		logger:      logger,
		notifier:    notifier,
		configMan:   config,
		stopChannel: make(chan bool),
		verbose:     verbose,
	}

	mixy, err := NewBLEIO(d, logger)
	if err != nil {
		logger.Errorw("Failed to create BLEIO", "error", err)
		return nil, fmt.Errorf("create new BLEIO: %w", err)
	}

	d.mixy = mixy

	sessionFinder, err := newSessionFinder(logger)
	if err != nil {
		logger.Errorw("Failed to create SessionFinder", "error", err)
		return nil, fmt.Errorf("create new SessionFinder: %w", err)
	}

	sessions, err := newSessionMapper(d, logger, sessionFinder)
	if err != nil {
		logger.Errorw("Failed to create sessionMapper", "error", err)
		return nil, fmt.Errorf("create new sessionMapper: %w", err)
	}

	d.sessions = sessions

	logger.Debug("Created meej instance")

	return d, nil
}

func (d *Meej) currConf() *Config {
	return &d.configMan.current
}

// Initialize sets up components and starts to run in the background
func (d *Meej) Initialize() error {
	d.logger.Debug("Initializing")

	// load the config for the first time
	if err := d.configMan.Load(); err != nil {
		d.logger.Errorw("Failed to load config during initialization", "error", err)
		return fmt.Errorf("load config during init: %w", err)
	}

	// initialize the session map
	if err := d.sessions.initialize(); err != nil {
		d.logger.Errorw("Failed to initialize session map", "error", err)
		return fmt.Errorf("init session map: %w", err)
	}

	d.setupInterruptHandler()

	if d.currConf().DisableTray {
		d.logger.Debugw("Running without tray icon", "reason", "disabled in config")

		// run in main thread while waiting on ctrl+C
		d.run()
	} else {
		d.runningWithTray = true
		d.initializeTray(d.run)
	}

	return nil
}

// SetVersion causes meej to add a version string to its tray menu if called before Initialize
func (d *Meej) SetVersion(version string) {
	d.version = version
}

// Verbose returns a boolean indicating whether meej is running in verbose mode
func (d *Meej) Verbose() bool {
	return d.verbose
}

func (d *Meej) setupInterruptHandler() {
	interruptChannel := util.SetupCloseHandler()

	go func() {
		signal := <-interruptChannel
		d.logger.Debugw("Interrupted", "signal", signal)
		d.signalStop()
	}()
}

func (d *Meej) run() {
	d.logger.Info("Run loop starting")

	go d.configMan.WatchConfigFileChanges()

	go func() {
		if err := d.mixy.Start(); err != nil {
			d.logger.Warnw("Failed to initiate Mixy connector", "error", err)
		}
	}()

	// wait until gracefully stopped
	<-d.stopChannel
	d.logger.Debug("Stop channel signaled, terminating")

	if err := d.stop(); err != nil {
		d.logger.Warnw("Failed to stop meej", "error", err)
		os.Exit(1)
	} else {
		os.Exit(0)
	}
}

func (d *Meej) signalStop() {
	d.logger.Debug("Signalling stop channel")
	d.stopChannel <- true
}

func (d *Meej) stop() error {
	d.logger.Info("Stopping")

	d.configMan.StopWatchingConfigFile()
	d.mixy.Stop()

	// release the session map
	if err := d.sessions.release(); err != nil {
		d.logger.Errorw("Failed to release session map", "error", err)
		return fmt.Errorf("release session map: %w", err)
	}

	if d.runningWithTray {
		d.stopTray()
	}

	// attempt to sync on exit - this won't necessarily work but can't harm
	_ = d.logger.Sync()

	return nil
}
