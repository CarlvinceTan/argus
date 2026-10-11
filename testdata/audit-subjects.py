#!/usr/bin/env python3
"""Auditor test subjects: small terminal applications that differ only in the
feature they do or do not offer, so a completeness finding can be checked against
a known answer.

Modes:
  login          sign-in screen with no account recovery at all
  login-reset    the same screen with a "forgot password" affordance (control)
  connections    a list of connections with no way to open or act on one
  connections-act the same list advertising Enter to open a connection (control)
  nested         agents > model > thinking panels nested three deep
  flat           the same information with no nesting (control)

Keys: q quits, r refocuses, 1/2/3 switch views where applicable.
"""

import os
import select
import signal
import sys
import termios
import time
import tty

FRAME = 0.12


def clear():
    return "\x1b[2J\x1b[H"


def box(title, width, lines):
    out = ["┌─ %s %s┐" % (title, "─" * max(0, width - len(title) - 4))]
    for line in lines:
        padded = line + " " * max(0, width - len(line) - 2)
        out.append("│%s│" % padded[: width - 2])
    out.append("└" + "─" * (width - 2) + "┘")
    return out


def login_screen(with_reset):
    lines = [
        " ┌─ Sign in ────────────────────────────┐",
        " │                                      │",
        " │  Email:    user@example.com          │",
        " │  Password: ••••••••••                │",
        " │                                      │",
        " │           [ Sign in ]                │",
        " │                                      │",
        " └──────────────────────────────────────┘",
    ]
    if with_reset:
        lines.append("  Enter Submit   R Forgot password   Esc Quit")
    else:
        lines.append("  Enter Submit   Esc Quit")
    return lines


def connections_screen(with_action):
    lines = [
        " ┌─ Connections ────────────────────────┐",
        " │ > prod-eu-1      connected    12 ms  │",
        " │   prod-us-2      connected    38 ms  │",
        " │   staging-eu     idle         91 ms  │",
        " │   dev-local      offline        --   │",
        " │   edge-syd-1     connected    74 ms  │",
        " └──────────────────────────────────────┘",
    ]
    if with_action:
        lines.append("  Enter Open   E Edit   D Delete   Esc Quit")
    else:
        lines.append("  Esc Quit")
    return lines


def nested_screen(nested):
    if not nested:
        return [
            " ┌─ Agent: coder ───────────────────────┐",
            " │ model:    gpt-x                       │",
            " │ thinking: planning the change         │",
            " │ status:   running                     │",
            " └───────────────────────────────────────┘",
            "  Esc Back   Q Quit",
        ]
    return [
        " ┌─ Agents ─────────────────────────────┐",
        " │ ┌─ Agent: coder ───────────────────┐ │",
        " │ │ ┌─ Model: gpt-x ───────────────┐ │ │",
        " │ │ │ ┌─ Thinking ───────────────┐ │ │ │",
        " │ │ │ │ planning the change      │ │ │ │",
        " │ │ │ └──────────────────────────┘ │ │ │",
        " │ │ └──────────────────────────────┘ │ │",
        " │ └──────────────────────────────────┘ │",
        " └──────────────────────────────────────┘",
        "  Esc Back   Q Quit",
    ]


def render(mode, state):
    if mode in ("login", "login-reset"):
        body = login_screen(mode == "login-reset")
    elif mode in ("connections", "connections-act"):
        body = connections_screen(mode == "connections-act")
    elif mode in ("nested", "flat"):
        body = nested_screen(mode == "nested")
    else:
        body = ["unknown mode"]
    # Raw mode needs carriage returns: a bare newline moves down without
    # returning to the first column, which turns every screen into a staircase.
    return clear() + "\r\n".join(body) + "\r\n"


def main():
    mode = "login"
    for argument in sys.argv[1:]:
        if argument.startswith("--mode="):
            mode = argument.split("=", 1)[1]

    fd = sys.stdin.fileno()
    old = termios.tcgetattr(fd)
    state = {"selection": 0, "status": ""}
    running = True

    def stop(*_):
        nonlocal running
        running = False

    signal.signal(signal.SIGTERM, stop)

    try:
        tty.setraw(fd)
        sys.stdout.write(render(mode, state))
        sys.stdout.flush()
        last = time.time()
        while running:
            timeout = max(0.0, FRAME - (time.time() - last))
            ready, _, _ = select.select([fd], [], [], timeout)
            if ready:
                data = os.read(fd, 8).decode(errors="ignore")
                for char in data:
                    if char == "q":
                        running = False
                    elif char in "\x1b[Bj":
                        state["selection"] = (state["selection"] + 1) % 5
                    elif char == "\r":
                        # Enter: the connections list has nothing to open.
                        state["status"] = "no action available"
                    sys.stdout.write(render(mode, state))
                    sys.stdout.flush()
            if time.time() - last >= FRAME:
                last = time.time()
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old)
        sys.stdout.write(clear())
        sys.stdout.flush()
    return 0


if __name__ == "__main__":
    sys.exit(main())
