package desktopmenu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/godbus/dbus/v5"
)

func Run(executable string) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("desktop session bus unavailable: %w", err)
	}
	defer conn.Close()
	result, err := conn.RequestName("io.memql.CockpitMenu", dbus.NameFlagDoNotQueue)
	if err != nil {
		return err
	}
	if result != dbus.RequestNameReplyPrimaryOwner {
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	systray.Run(func() {
		systray.SetIcon(icon)
		systray.SetTitle("MemQL Cockpit")
		go menuLoop(ctx, executable)
		go func() { <-ctx.Done(); systray.Quit() }()
	}, cancel)
	return nil
}

type menuAction struct{ kind, id string }
type homeMenu struct{ title, open, toggle *systray.MenuItem }

func menuLoop(ctx context.Context, executable string) {
	actions := make(chan menuAction, 16)
	addAction := func(item *systray.MenuItem, action menuAction) {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-item.ClickedCh:
					select {
					case actions <- action:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	heading := systray.AddMenuItem("Connecting to background worker…", "")
	heading.Disable()
	problem := systray.AddMenuItem("", "")
	problem.Disable()
	problem.Hide()
	systray.AddSeparator()
	servers := systray.AddMenuItem("Servers", "")
	servers.Disable()
	systray.AddSeparator()
	homes := map[string]homeMenu{}
	state := map[string]Home{}
	logs := systray.AddMenuItem("Open worker logs", "")
	addAction(logs, menuAction{kind: "logs"})
	retry := systray.AddMenuItem("Retry worker connection", "")
	addAction(retry, menuAction{kind: "refresh"})
	quit := systray.AddMenuItem("Quit menu (worker keeps running)", "")
	addAction(quit, menuAction{kind: "quit"})
	refresh := func() {
		reply, err := request(executable, "--action=status")
		if err != nil {
			state = map[string]Home{}
			servers.Disable()
			heading.SetTitle("Background worker unavailable")
			problem.SetTitle(err.Error())
			problem.Show()
			for _, m := range homes {
				m.toggle.Disable()
				m.open.Disable()
			}
			return
		}
		problem.Hide()
		heading.SetTitle(summary(reply.Status))
		systray.SetTooltip("MemQL Cockpit · " + summary(reply.Status))
		state = map[string]Home{}
		if reply.Status == nil {
			servers.Disable()
			return
		}
		servers.Disable()
		if len(reply.Status.Homes) > 0 {
			servers.Enable()
		}
		for _, h := range reply.Status.Homes {
			state[h.ID] = h
			m, ok := homes[h.ID]
			if !ok {
				m.title = servers.AddSubMenuItem(h.Server, "")
				m.open = m.title.AddSubMenuItem("Open MemQL OS", "")
				m.toggle = m.title.AddSubMenuItem("Pause connection", "")
				homes[h.ID] = m
				addAction(m.open, menuAction{kind: "open", id: h.ID})
				addAction(m.toggle, menuAction{kind: "toggle", id: h.ID})
			}
			m.title.SetTitle(h.Server + " · " + h.State)
			m.title.Show()
			m.toggle.Enable()
			if h.Enabled {
				m.toggle.SetTitle("Pause connection")
			} else {
				m.toggle.SetTitle("Allow connecting")
			}
			if _, err := safeOSURL(h.OSURL); err == nil {
				m.open.Enable()
			} else {
				m.open.Disable()
			}
		}
		for id, m := range homes {
			if _, ok := state[id]; !ok {
				m.title.Hide()
			}
		}
	}
	refresh()
	logPath := ""
	defer func() {
		if logPath != "" {
			_ = os.Remove(logPath)
		}
	}()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		case action := <-actions:
			problem.Hide()
			var err error
			switch action.kind {
			case "quit":
				systray.Quit()
				return
			case "refresh":
				refresh()
			case "toggle":
				if h, ok := state[action.id]; ok {
					_, err = request(executable, "--action=home", "--home="+h.ID, fmt.Sprintf("--enabled=%t", !h.Enabled))
					refresh()
				}
			case "open":
				if h, ok := state[action.id]; ok {
					var address string
					address, err = safeOSURL(h.OSURL)
					if err == nil {
						err = openDesktop(address)
					}
				}
			case "logs":
				// Ask the worker's existing redacted log reader; never open raw credentials.
				cmdCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
				data, readErr := exec.CommandContext(cmdCtx, executable, "worker", "control", "--action=logs").Output()
				cancel()
				err = readErr
				if err == nil {
					var file *os.File
					if logPath == "" {
						file, err = os.CreateTemp("", "memql-worker-logs-*.json")
						if err == nil {
							logPath = file.Name()
						}
					} else {
						file, err = os.OpenFile(logPath, os.O_WRONLY|os.O_TRUNC, 0600)
					}
					if err == nil {
						_, err = file.Write(data)
						file.Close()
						if err == nil {
							err = openDesktop(file.Name())
						}
					}
				}
			}
			if err != nil {
				problem.SetTitle(err.Error())
				problem.Show()
			}
		}
	}
}
func openDesktop(target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "xdg-open", target).Run()
}
