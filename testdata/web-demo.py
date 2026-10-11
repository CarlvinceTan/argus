#!/usr/bin/env python3
"""Web auditor test subjects.

Each mode is the same small page with one feature added or removed, so a
completeness finding can be checked against a known answer:

  login          sign-in form with no recovery path at all
  login-reset    the same form with a "Forgot password?" link (control)
  connections    a table of connections; rows are not clickable and nothing acts
  connections-act the same table with a link per row and an Edit action (control)
  nested         three nested bordered panels (control for the structure check)

Served over HTTP by any static server the caller chooses.
"""

import http.server
import socketserver
import sys

PAGE = """<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>{title}</title>
  <style>
    body {{ font-family: system-ui, sans-serif; background: #17181c; color: #e6e9ef; margin: 0; padding: 32px; }}
    .panel {{ border: 1px solid #2c3040; border-radius: 10px; padding: 20px; max-width: 520px; }}
    .panel .panel {{ border-color: #3a4058; margin-top: 12px; }}
    .panel .panel .panel {{ border-color: #4a5270; margin-top: 12px; }}
    label {{ display: block; margin: 10px 0 4px; font-size: 13px; color: #9aa4b8; }}
    input {{ width: 100%; padding: 8px; border-radius: 6px; border: 1px solid #2c3040; background: #101216; color: #e6e9ef; }}
    button, .action {{ margin-top: 14px; padding: 8px 14px; border-radius: 6px; border: 1px solid #34508a; background: #1d2b45; color: #dce6ff; cursor: pointer; text-decoration: none; display: inline-block; }}
    table {{ border-collapse: collapse; width: 100%; font-size: 13px; }}
    th, td {{ text-align: left; padding: 6px 10px; border-bottom: 1px solid #262a38; }}
    .muted {{ color: #6f7789; font-size: 13px; }}
    .error {{ color: #ff8080; }}
  </style>
</head>
<body>
{body}
</body>
</html>
"""

LOGIN = """
<h1>Sign in</h1>
<div class="panel">
  <form onsubmit="event.preventDefault();">
    <label for="email">Email</label>
    <input id="email" type="email" placeholder="you@example.com">
    <label for="password">Password</label>
    <input id="password" type="password" placeholder="Password">
    <button type="submit">Sign in</button>
    {reset}
  </form>
</div>
"""

CONNECTIONS = """
<h1>Connections</h1>
<p class="muted">{count} connections</p>
<div class="panel">
  <table>
    <thead><tr><th>Name</th><th>Status</th><th>Latency</th>{actions}</tr></thead>
    <tbody>
      {rows}
    </tbody>
  </table>
  {footer}
</div>
"""


def connections_page(with_actions: bool) -> str:
    names = ["prod-eu-1", "prod-us-2", "staging-eu", "dev-local", "edge-syd-1"]
    rows = ""
    for index, name in enumerate(names):
        if with_actions:
            rows += (
                f"<tr><td><a href=\"/connections/{name}\">{name}</a></td>"
                f"<td>connected</td><td>{12 + index * 7} ms</td>"
                f"<td><a class=\"action\" href=\"/connections/{name}/edit\">Edit</a></td></tr>"
            )
        else:
            rows += f"<tr><td>{name}</td><td>connected</td><td>{12 + index * 7} ms</td></tr>"
    return CONNECTIONS.format(
        count=len(names),
        actions="<th></th>" if with_actions else "",
        rows=rows,
        footer="",
    )


def nested_page(nested: bool) -> str:
    inner = "<p>Thinking: planning the change</p>"
    if nested:
        inner = (
            '<div class="panel"><h3>Thinking</h3><p>planning the change</p></div>'
        )
        inner = f'<div class="panel"><h3>Model: gpt-x</h3>{inner}</div>'
        inner = f'<div class="panel"><h3>Agent: coder</h3>{inner}</div>'
        inner = f'<h2>Agents</h2><div class="panel">{inner}</div>'
    else:
        inner = (
            "<h2>Agent: coder</h2><div class=\"panel\">"
            "<p>model: gpt-x</p><p>thinking: planning the change</p><p>status: running</p></div>"
        )
    return inner


def render(mode: str) -> str:
    if mode == "login":
        return PAGE.format(title="Sign in", body=LOGIN.format(reset=""))
    if mode == "login-reset":
        return PAGE.format(
            title="Sign in",
            body=LOGIN.format(reset='<p><a href="/forgot">Forgot password?</a></p>'),
        )
    if mode == "connections":
        return PAGE.format(title="Connections", body=connections_page(False))
    if mode == "connections-act":
        return PAGE.format(title="Connections", body=connections_page(True))
    if mode == "nested":
        return PAGE.format(title="Agents", body=nested_page(True))
    if mode == "flat":
        return PAGE.format(title="Agent", body=nested_page(False))
    return PAGE.format(title="Unknown", body="<h1>unknown mode</h1>")


class Handler(http.server.BaseHTTPRequestHandler):
    mode = "login"

    def do_GET(self):  # noqa: N802
        body = render(self.mode).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):  # keep the console quiet
        return


def main() -> int:
    mode = "login"
    port = 8000
    for argument in sys.argv[1:]:
        if argument.startswith("--mode="):
            mode = argument.split("=", 1)[1]
        if argument.startswith("--port="):
            port = int(argument.split("=", 1)[1])
    Handler.mode = mode
    with socketserver.TCPServer(("127.0.0.1", port), Handler) as server:
        print(f"web-demo serving {mode} on http://127.0.0.1:{port}/", flush=True)
        server.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
