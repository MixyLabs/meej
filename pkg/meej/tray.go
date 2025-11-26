package meej

import (
	"fyne.io/systray"

	"github.com/MixyLabs/meej/pkg/meej/util"
)

func (d *Meej) initializeTray(onDone func()) {
	logger := d.logger.Named("tray")

	onReady := func() {
		logger.Debug("Tray instance ready")

		systray.SetTemplateIcon(MeejLogoIconData, MeejLogoIconData)
		systray.SetTitle("meej")
		systray.SetTooltip("meej")

		editConfig := systray.AddMenuItem("Edit configuration", "Open config file with notepad")

		refreshSessions := systray.AddMenuItem("Re-scan audio sessions", "Manually refresh audio sessions if something's stuck")

		if d.version != "" {
			systray.AddSeparator()
			versionInfo := systray.AddMenuItem(d.version, "")
			versionInfo.Disable()
		}

		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop meej and quit")

		go func() {
			for {
				select {
				case <-quit.ClickedCh:
					logger.Info("Quit menu item clicked, stopping")

					d.signalStop()

				case <-editConfig.ClickedCh:
					logger.Info("Edit config menu item clicked, opening config for editing")

					// TODO: make editor configurable
					editor := "notepad.exe"
					if util.Linux() {
						editor = "gedit"
					}

					if err := util.OpenExternal(logger, editor, userConfigFilepath); err != nil {
						logger.Warnw("Failed to open config file for editing", "error", err)
					}

				case <-refreshSessions.ClickedCh:
					logger.Info("Refresh sessions menu item clicked, triggering session map refresh")
					//d.sessions.refreshSessions(true)
				}
			}
		}()

		onDone()
	}

	onExit := func() {
		logger.Debug("Tray exited")
	}

	logger.Debug("Running in tray")
	systray.Run(onReady, onExit)
}

func (d *Meej) stopTray() {
	d.logger.Debug("Quitting tray")
	systray.Quit()
}
