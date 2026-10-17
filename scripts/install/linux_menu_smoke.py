"""Exercise the built Linux tray on a private session bus, without a desktop."""
import argparse
import pathlib
import select
import subprocess
import sys
import time

import dbus
import dbus.service
from dbus.mainloop.glib import DBusGMainLoop
from gi.repository import GLib


def watcher():
    DBusGMainLoop(set_as_default=True)
    bus = dbus.SessionBus()
    name = dbus.service.BusName("org.kde.StatusNotifierWatcher", bus)

    class Watcher(dbus.service.Object):
        @dbus.service.method("org.kde.StatusNotifierWatcher", in_signature="s", out_signature="", sender_keyword="sender")
        def RegisterStatusNotifierItem(self, path, sender=None):
            print(sender + " " + path, flush=True)

    service = Watcher(name, "/StatusNotifierWatcher")
    print("ready", flush=True)
    GLib.MainLoop().run()
    service.remove_from_connection()


def line(process):
    ready, _, _ = select.select([process.stdout], [], [], 15)
    if not ready:
        raise AssertionError("tray did not register with the desktop watcher")
    result = process.stdout.readline().decode().strip()
    assert result, "tray/watcher exited before registration"
    return result


def stop(process):
    if process and process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def inspect(binary):
    bus = dbus.SessionBus()
    processes = []

    def start_watcher():
        # Readiness and registration can arrive together after a panel restart.
        # A buffered text reader can consume both, leaving select() waiting on
        # an empty pipe while the registration is already in Python's buffer.
        process = subprocess.Popen([sys.executable, __file__, "--watcher"], stdout=subprocess.PIPE, bufsize=0)
        processes.append(process)
        assert line(process) == "ready"
        return process

    try:
        host = start_watcher()
        menu = subprocess.Popen([binary, "menu"])
        processes.append(menu)
        sender, path = line(host).split()
        properties = dbus.Interface(bus.get_object(sender, path), "org.freedesktop.DBus.Properties")
        assert str(properties.Get("org.kde.StatusNotifierItem", "Title")) == "MemQL Cockpit"
        icon = properties.Get("org.kde.StatusNotifierItem", "IconPixmap")[0]
        assert (int(icon[0]), int(icon[1]), len(icon[2])) == (64, 64, 64 * 64 * 4)
        menu_path = properties.Get("org.kde.StatusNotifierItem", "Menu")
        desktop_menu = dbus.Interface(bus.get_object(sender, str(menu_path)), "com.canonical.dbusmenu")
        deadline = time.monotonic() + 15
        while True:
            _, layout = desktop_menu.GetLayout(0, -1, [])
            rendered = str(layout)
            if "Background worker unavailable" in rendered:
                break
            assert time.monotonic() < deadline, rendered
            time.sleep(0.1)
        assert "Open worker logs" in rendered and "Quit menu" in rendered
        # A second launch must exit, rather than adding a duplicate icon.
        subprocess.run([binary, "menu"], timeout=5, check=True)
        assert menu.poll() is None
        # COSMIC/panel restarts must not require restarting Cockpit.
        stop(host)
        host = start_watcher()
        assert line(host).split() == [sender, path]
        menu.terminate()
        assert menu.wait(timeout=5) == 0
        print("PASS: tray registration, icon, menu, missing-worker state, singleton, watcher restart and graceful quit")
    finally:
        for process in reversed(processes):
            stop(process)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--watcher", action="store_true")
    parser.add_argument("--binary", type=pathlib.Path)
    args = parser.parse_args()
    if args.watcher:
        watcher()
    elif args.binary:
        inspect(str(args.binary.resolve()))
    else:
        parser.error("--binary is required")


if __name__ == "__main__":
    main()
