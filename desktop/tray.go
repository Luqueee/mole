package main

import (
	_ "embed"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// The mascot is an opaque rounded square, so a template image (alpha only)
// would draw as a solid white block in the menu bar; use the colour icon.
//
//go:embed build/tray/color.png
var trayColorIcon []byte

// setupTray adds a system tray icon with the daemon's state and controls,
// and makes closing the window hide it instead of quitting: mole is a
// background tool, so the app keeps running from the tray until Quit.
func setupTray(app *application.App, win *application.WebviewWindow, svc *MoleService) {
	var quitting atomic.Bool

	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if quitting.Load() {
			return
		}
		e.Cancel()
		win.Hide()
	})

	show := func() {
		win.Show()
		win.Focus()
	}

	tray := app.SystemTray.New()
	tray.SetIcon(trayColorIcon)

	menu := app.NewMenu()
	state := menu.Add("mole")
	state.SetEnabled(false)
	incoming := menu.Add("")
	incoming.SetHidden(true)
	incoming.OnClick(func(*application.Context) { show() })
	menu.AddSeparator()
	menu.Add("Show mole").OnClick(func(*application.Context) { show() })
	start := menu.Add("Start")
	stop := menu.Add("Stop")
	restart := menu.Add("Restart")
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(*application.Context) {
		quitting.Store(true)
		app.Quit()
	})
	tray.SetMenu(menu)
	tray.OnDoubleClick(show)

	act := func(fn func() error) func(*application.Context) {
		return func(*application.Context) {
			go func() {
				if err := fn(); err != nil {
					app.Logger.Warn("tray action failed", "err", err)
				}
				refreshTray(tray, menu, state, incoming, start, stop, svc)
			}()
		}
	}
	start.OnClick(act(svc.Start))
	stop.OnClick(act(svc.Stop))
	restart.OnClick(act(svc.Restart))

	refreshTray(tray, menu, state, incoming, start, stop, svc)
	go func() {
		for range time.Tick(3 * time.Second) {
			refreshTray(tray, menu, state, incoming, start, stop, svc)
		}
	}()
}

func refreshTray(tray *application.SystemTray, menu *application.Menu, state, incoming, start, stop *application.MenuItem, svc *MoleService) {
	st := svc.GetStatus()
	switch {
	case st.Running:
		state.SetLabel(fmt.Sprintf("● LIVE · %s · %d ports", st.Remote, len(st.Ports)))
		tray.SetLabel(fmt.Sprint(len(st.Ports)))
		tray.SetTooltip(fmt.Sprintf("mole: %s, %d ports, %d active", st.Remote, len(st.Ports), st.Stats.ActiveConns))
	default:
		state.SetLabel("○ OFFLINE")
		tray.SetLabel("")
		tray.SetTooltip("mole: stopped")
	}
	start.SetEnabled(!st.Running)
	stop.SetEnabled(st.Running)
	// Files offered by the remote wait for an answer: say so where the user
	// will see it even with the window hidden.
	if n := svc.pendingCount(); n > 0 && st.Running {
		incoming.SetLabel(fmt.Sprintf("⬇ %d incoming transfer(s) waiting…", n))
		incoming.SetHidden(false)
		tray.SetLabel(fmt.Sprintf("%d ⬇%d", len(st.Ports), n))
	} else {
		incoming.SetHidden(true)
	}
	menu.Update()
}
