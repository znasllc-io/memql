//go:build !linux

package desktopmenu

import "errors"

func Run(string) error {
	return errors.New("the Linux tray runs on Linux; macOS uses the native MemQL menu app")
}
