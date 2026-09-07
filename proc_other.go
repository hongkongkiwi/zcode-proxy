//go:build !windows

package main

import (
	"os/exec"
)

// killProcessByName 非 Windows: pkill
func killProcessByName(name string) (string, error) {
	out, err := exec.Command("pkill", "-f", name).CombinedOutput()
	return string(out), err
}

// runHidden 非 Windows 直接执行
func runHidden(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
