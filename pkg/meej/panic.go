package meej

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/MixyLabs/meej/pkg/meej/util"
)

const (
	crashlogFilename        = "meej-crash-%s.log"
	crashlogTimestampFormat = "2006.01.02-15.04.05"

	crashMessage = `-----------------------------------------------------------------
                        meej crashlog
-----------------------------------------------------------------
Unfortunately, meej has crashed.
To help diagnose the issue, a crashlog has been generated.
Please consider sharing this file with developers to help improve meej.
You can do so by opening an issue at: https://github.com/MixyLabs/meej/issues/new
-----------------------------------------------------------------
Time: %s
Panic occurred: %s
Stack trace:
%s
-----------------------------------------------------------------
`
)

func (d *Meej) recoverFromPanic() {
	r := recover()

	if r == nil {
		return
	}

	now := time.Now()

	if err := util.EnsureDirExists(logDirectory); err != nil {
		panic(fmt.Errorf("ensure crashlog dir exists: %w", err))
	}

	crashlogBytes := bytes.NewBufferString(fmt.Sprintf(crashMessage, now.Format(crashlogTimestampFormat), r, debug.Stack()))
	crashlogPath := filepath.Join(logDirectory, fmt.Sprintf(crashlogFilename, now.Format(crashlogTimestampFormat)))

	if err := os.WriteFile(crashlogPath, crashlogBytes.Bytes(), os.ModePerm); err != nil {
		panic(fmt.Errorf("can't even write the crashlog file contents: %w", err))
	}

	d.logger.Errorw("Encountered and logged panic, crashing",
		"crashlogPath", crashlogPath,
		"error", r)

	d.notifier.Notify("Unexpected crash occurred...",
		fmt.Sprintf("More details in %s", crashlogPath))

	d.signalStop()
	d.logger.Errorw("Quitting", "exitCode", 1)
	os.Exit(1)
}
