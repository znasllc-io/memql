package desktopmenu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/godbus/dbus/v5"
)

func Run(executable, version, variant string) error {
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
		go menuLoop(ctx, executable, version, variant)
		go func() { <-ctx.Done(); systray.Quit() }()
	}, cancel)
	return nil
}

type menuAction struct{ kind, id string }
type homeMenu struct{ title, detail, open, toggle *systray.MenuItem }
type updateResult struct {
	kind  string
	reply updateReply
	err   error
}

func menuLoop(ctx context.Context, executable, version, variant string) {
	actions := make(chan menuAction, 16)
	updates := make(chan updateResult, 4)
	update := updateState{}
	progressPending := false
	workerSummary := "Connecting to background worker…"
	badgedIcon := updateIcon()
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
	systray.AddSeparator()
	updateStatus := systray.AddMenuItem("Updates have not been checked", "")
	updateStatus.Disable()
	updateProblem := systray.AddMenuItem("", "")
	updateProblem.Disable()
	updateProblem.Hide()
	checkUpdate := systray.AddMenuItem("Check for updates", "")
	addAction(checkUpdate, menuAction{kind: "check-update"})
	applyUpdate := systray.AddMenuItem("Update and restart", "Your saved servers and connection choices are kept")
	applyUpdate.Hide()
	addAction(applyUpdate, menuAction{kind: "install-update"})
	about := systray.AddMenuItem("About MemQL Cockpit", "")
	aboutInstalled := about.AddSubMenuItem(aboutVersion(version, variant), "Installed Cockpit build")
	aboutInstalled.Disable()
	aboutPlatform := about.AddSubMenuItem("Linux · "+runtime.GOARCH, "")
	aboutPlatform.Disable()
	aboutWorker := about.AddSubMenuItem("Worker version unavailable", "")
	aboutWorker.Disable()
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit menu (worker keeps running)", "")
	addAction(quit, menuAction{kind: "quit"})
	renderUpdate := func() {
		p := update.presentation()
		updateStatus.SetTitle(p.status)
		checkUpdate.SetTitle(p.checkLabel)
		if p.canCheck {
			checkUpdate.Enable()
		} else {
			checkUpdate.Disable()
		}
		applyUpdate.SetTitle(p.installLabel)
		if p.showInstall {
			applyUpdate.Show()
		} else {
			applyUpdate.Hide()
		}
		if p.canInstall {
			applyUpdate.Enable()
		} else {
			applyUpdate.Disable()
		}
		if p.problem == "" {
			updateProblem.Hide()
		} else {
			updateProblem.SetTitle(p.problem)
			updateProblem.Show()
		}
		if update.installing {
			quit.Disable()
		} else {
			quit.Enable()
		}
		for id, home := range homes {
			if _, known := state[id]; known && !update.installing {
				home.toggle.Enable()
			} else {
				home.toggle.Disable()
			}
		}
		if p.badge {
			systray.SetIcon(badgedIcon)
			systray.SetTitle("MemQL Cockpit — update available")
		} else {
			systray.SetIcon(icon)
			systray.SetTitle("MemQL Cockpit")
		}
		systray.SetTooltip("MemQL Cockpit · " + workerSummary + " · " + p.status)
	}
	deliverUpdate := func(result updateResult) {
		select {
		case updates <- result:
		case <-ctx.Done():
		}
	}
	check := func() {
		if !update.beginCheck() {
			return
		}
		renderUpdate()
		go func() {
			checkCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
			defer cancel()
			reply, err := requestUpdate(checkCtx, executable, "--check")
			deliverUpdate(updateResult{"check", reply, err})
		}()
	}
	refresh := func() {
		reply, err := request(executable, "--action=status")
		if err != nil {
			state = map[string]Home{}
			servers.Disable()
			heading.SetTitle("Background worker unavailable")
			workerSummary = "Background worker unavailable"
			aboutWorker.SetTitle("Worker version unavailable")
			renderUpdate()
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
		workerSummary = summary(reply.Status)
		state = map[string]Home{}
		if reply.Status == nil {
			servers.Disable()
			aboutWorker.SetTitle("Worker version unavailable")
			for _, m := range homes {
				m.open.Disable()
			}
			renderUpdate()
			return
		}
		aboutWorker.SetTitle("Worker " + reply.Status.Version)
		servers.Disable()
		if len(reply.Status.Homes) > 0 {
			servers.Enable()
		}
		for _, h := range reply.Status.Homes {
			state[h.ID] = h
			m, ok := homes[h.ID]
			if !ok {
				m.title = servers.AddSubMenuItem(h.Server, "")
				m.detail = m.title.AddSubMenuItem("", "")
				m.detail.Disable()
				m.detail.Hide()
				m.open = m.title.AddSubMenuItem("Open MemQL OS", "")
				m.toggle = m.title.AddSubMenuItem("Pause connection", "")
				homes[h.ID] = m
				addAction(m.open, menuAction{kind: "open", id: h.ID})
				addAction(m.toggle, menuAction{kind: "toggle", id: h.ID})
			}
			m.title.SetTitle(h.Server + " · " + h.State)
			m.title.Show()
			if update.installing {
				m.toggle.Disable()
			} else {
				m.toggle.Enable()
			}
			label, detail, _ := homeConnectionAction(h)
			m.toggle.SetTitle(label)
			if detail == "" {
				m.detail.Hide()
			} else {
				m.detail.SetTitle(detail)
				m.detail.Show()
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
		renderUpdate()
	}
	refresh()
	check()
	logPath := ""
	defer func() {
		if logPath != "" {
			_ = os.Remove(logPath)
		}
	}()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	updateTicker := time.NewTicker(time.Hour)
	defer updateTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
			if update.installing && !progressPending {
				progressPending = true
				go func() {
					statusCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
					defer cancel()
					reply, err := requestUpdate(statusCtx, executable, "--status")
					deliverUpdate(updateResult{"progress", reply, err})
				}()
			}
		case <-updateTicker.C:
			check()
		case result := <-updates:
			switch result.kind {
			case "check":
				update.checked(result.reply, result.err)
			case "progress":
				progressPending = false
				if result.err == nil {
					update.progressChanged(result.reply)
				}
			case "install":
				update.installed(result.reply, result.err)
				if result.reply.Updated {
					aboutInstalled.SetTitle(aboutVersion(result.reply.CurrentVersion, variant))
				}
				if result.err == nil && result.reply.Updated {
					// Replace this menu process only after the installer has
					// completed. Exec keeps systemd's main PID; a failed relaunch
					// leaves this menu available with an explicit remedy.
					if err := syscall.Exec(executable, []string{executable, "menu"}, os.Environ()); err != nil {
						update.problem = "Cockpit updated, but the menu could not restart. Quit and reopen MemQL Cockpit."
						aboutInstalled.SetTitle(aboutVersion(result.reply.CurrentVersion, variant))
					}
				}
			}
			renderUpdate()
		case action := <-actions:
			problem.Hide()
			var err error
			switch action.kind {
			case "quit":
				if update.installing {
					continue
				}
				systray.Quit()
				return
			case "check-update":
				check()
			case "install-update":
				if target, ok := update.beginInstall(); ok {
					renderUpdate()
					go func() {
						reply, err := installUpdate(executable, target)
						deliverUpdate(updateResult{"install", reply, err})
					}()
				}
			case "refresh":
				refresh()
			case "toggle":
				if update.installing {
					continue
				}
				if h, ok := state[action.id]; ok {
					_, _, enabled := homeConnectionAction(h)
					_, err = request(executable, "--action=home", "--home="+h.ID, fmt.Sprintf("--enabled=%t", enabled))
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
