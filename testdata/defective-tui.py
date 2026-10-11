#!/usr/bin/env python3
"""A deliberately defective TUI used to calibrate Argus.

Every defect here is one a real terminal program has shipped at least once:
a broken border, a duplicated line, clipped text, low-contrast text, a progress
bar that disagrees with its label, a spinner that never advances, a flickering
cell, an empty list with no guidance, and no visible selection indicator.

Keys: q quit, h help, arrows/tab move a hidden selection, a toggles "all".
"""

import os
import select
import sys
import termios
import time
import tty

FRAME_INTERVAL = 0.08
FLICKER = True


def size():
    try:
        cols, rows = os.get_terminal_size(sys.stdout.fileno())
    except OSError:
        cols, rows = 100, 30
    return cols, rows


def draw(state, tick):
    cols, rows = size()
    out = []
    out.append("\x1b[2J\x1b[H")
    out.append("\x1b[1mTask Runner\x1b[0m\r\n")
    # Broken border: the left edge is missing on purpose.
    out.append("\r\n")
    out.append("  Tasks" + "─" * 30 + "┐\r\n")
    for index, task in enumerate(state["tasks"]):
        marker = ">" if state["selected"] and index == state["hidden_sel"] else " "
        out.append("%s %s%*s│\r\n" % (marker, task, max(0, 30 - len(task)), ""))
    out.append("  " + "─" * 30 + "┘\r\n")
    # Duplicated line.
    out.append("\r\nLoading configuration...\r\n")
    out.append("Loading configuration...\r\n")
    # Clipped text: long value cut at the edge with no ellipsis.
    out.append("\r\n" + "path: " + "/very/long/path/that/keeps/going/and/does/not/stop/ever/really/and/keeps/going/beyond/the/terminal/width" + "\r\n")
    # Low-contrast text (dark grey on black).
    out.append("\x1b[38;5;236msecondary status text that is almost invisible\x1b[0m\r\n")
    # Progress bar at 40% that claims 90%.
    bar = "█" * 12 + "░" * 18
    out.append("\r\nDownloading " + bar + " 90%\r\n")
    # Spinner that never advances while it claims to be loading.
    out.append("\r\n⠋ loading data from disk\r\n")
    # Flickering cell, alternating every frame.
    flicker = "\x1b[7m REC \x1b[0m" if tick % 2 == 0 else "     "
    out.append("\r\nstatus: " + flicker + "\r\n")
    # Empty list with no guidance.
    out.append("\r\nRecent runs" + "─" * 24 + "\r\n")
    out.append("(none)\r\n")
    # Footer hints that reference keys.
    out.append("\r\n^Q Quit   H Help   A Toggle all   \u2191\u2193 Select\r\n")
    if state["screen"] == "help":
        out.append("\r\n\x1b[1mHelp\x1b[0m\r\nThis help screen has no way back that it mentions.\r\n")
    sys.stdout.write("".join(out))
    sys.stdout.flush()


def main():
    fd = sys.stdin.fileno()
    old = termios.tcgetattr(fd)
    state = {
        "tasks": ["build assets", "run tests", "deploy staging"],
        "selected": True,
        "hidden_sel": 0,
        "screen": "main",
    }
    tick = 0
    last = 0.0
    try:
        tty.setraw(fd)
        while True:
            now = time.time()
            timeout = max(0.0, FRAME_INTERVAL - (now - last))
            ready, _, _ = select.select([fd], [], [], timeout)
            if ready:
                data = os.read(fd, 8)
                for char in data.decode(errors="ignore"):
                    if char == "q":
                        sys.stdout.write("\x1b[2J\x1b[H")
                        sys.stdout.flush()
                        return 0
                    if char == "h":
                        state["screen"] = "help" if state["screen"] == "main" else "main"
                    if char == "a":
                        state["selected"] = not state["selected"]
                    if char in "\x1b[Bj":
                        state["hidden_sel"] = (state["hidden_sel"] + 1) % len(state["tasks"])
            if time.time() - last >= FRAME_INTERVAL:
                draw(state, tick)
                tick += 1
                last = time.time()
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old)


if __name__ == "__main__":
    sys.exit(main())
