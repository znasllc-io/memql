package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/znasllc-io/memql/cockpit/internal/desktopmenu"
)

func handleMenu(args []string) {
	fs := flag.NewFlagSet("menu", flag.ExitOnError)
	install := fs.Bool("install", false, "Install the Linux tray and start it at desktop sign-in")
	uninstall := fs.Bool("uninstall", false, "Stop and remove the Linux tray, keeping the worker")
	start := fs.Bool("start", false, "Start the installed tray in this desktop session")
	fs.Parse(args)
	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "macOS uses the native MemQL menu app")
		os.Exit(2)
	}
	count := 0
	for _, set := range []bool{*install, *uninstall, *start} {
		if set {
			count++
		}
	}
	if count > 1 || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "choose one of --install, --uninstall, or --start")
		os.Exit(2)
	}
	executable, err := os.Executable()
	if err == nil {
		switch {
		case *install:
			err = desktopmenu.Install(executable)
		case *uninstall:
			err = desktopmenu.Uninstall()
		case *start:
			err = desktopmenu.Start()
		default:
			err = desktopmenu.Run(executable)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
