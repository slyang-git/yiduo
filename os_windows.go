//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func stopDaemonOS() error {
	pid, err := readDaemonPid()
	if err != nil {
		return err
	}
	if pid == 0 {
		return fmt.Errorf("daemon pid not found")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Kill(); err != nil {
		return err
	}
	return nil
}

func daemonRunningOS() (bool, int) {
	pid, err := readDaemonPid()
	if err != nil || pid == 0 {
		return false, 0
	}
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid)).Output()
	if err != nil {
		return false, 0
	}
	text := string(out)
	if strings.Contains(text, "No tasks are running") {
		return false, 0
	}
	if strings.Contains(text, fmt.Sprintf("%d", pid)) {
		return true, pid
	}
	return false, 0
}

func detectDiskFreeGBOS(path string) int {
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	_ = path
	return 0
}

func daemonSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
