package util

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"syscall"
)

func getCurrentWindowProcessNames() ([]string, error) {
	return nil, errors.New("Not implemented")
}

func CreateMutex(name string) error {
	lockFile := name + ".lock"
	currentPid := os.Getpid()

	lockContent, err := ioutil.ReadFile(lockFile)
	if err == nil {
		if len(lockContent) > 0 && string(lockContent) != fmt.Sprintf("%d", currentPid) {
			lockProcessId, _ := strconv.Atoi(string(lockContent))
			process, err := os.FindProcess(lockProcessId)
			if err == nil {
				pSignal := process.Signal(syscall.Signal(0))
				if pSignal == nil {
					return fmt.Errorf("another instance of fastfinder is running")
				}
			}
		}
	}

	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0664)
	if err != nil {
		return fmt.Errorf("cannot instantiate mutex")
	}
	defer f.Close()
	_, err = f.Write([]byte(fmt.Sprintf("%d", currentPid)))
	if err != nil {
		return fmt.Errorf("cannot instantiate mutex")
	}

	return nil
}
