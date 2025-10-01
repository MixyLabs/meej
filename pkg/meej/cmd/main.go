package main

import (
	"flag"
	"fmt"

	"github.com/MixyLabs/meej/pkg/meej"
)

var (
	gitCommit  string
	versionTag string
	buildType  string

	verbose bool
)

func init() {
	flag.BoolVar(&verbose, "verbose", false, "show verbose logs (useful for debugging BLE)")
	flag.BoolVar(&verbose, "v", false, "shorthand for --verbose")
	flag.Parse()
}

func main() {
	logger, err := meej.NewLogger(buildType)
	if err != nil {
		panic(fmt.Sprintf("Failed to create logger: %v", err))
	}

	named := logger.Named("main")
	named.Debug("Created logger")

	named.Infow("Version info",
		"gitCommit", gitCommit,
		"versionTag", versionTag,
		"buildType", buildType)

	if verbose {
		named.Debug("Verbose flag provided, all log messages will be shown")
	}

	d, err := meej.NewMeej(logger, verbose)
	if err != nil {
		named.Fatalw("Failed to create meej object", "error", err)
	}

	if buildType != "" && (versionTag != "" || gitCommit != "") {
		identifier := gitCommit
		if versionTag != "" {
			identifier = versionTag
		}

		versionString := fmt.Sprintf("Version %s-%s", buildType, identifier)
		d.SetVersion(versionString)
	}

	if err = d.Initialize(); err != nil {
		named.Fatalw("Failed to initialize meej", "error", err)
	}
}
