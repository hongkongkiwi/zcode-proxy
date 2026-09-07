//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// killProcessByName Windows: taskkill 隐藏窗口执行
func killProcessByName(name string) (string, error) {
	cmd := exec.Command("taskkill", "/IM", name, "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runHidden Windows 下隐藏窗口执行任意命令
func runHidden(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
