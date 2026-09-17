# Web SSH

A terminal in the browser: a shell inside the user's own container, opened from
the Machine card (next to Start / Stop / Restart) in the user panel.

It is not an SSH client. The container runs on the panel host, so the panel
talks to it through the Incus exec API over the daemon's unix socket, with a
PTY. Nothing listens on the container side, there are no keys to hand out, and
there is no extra network path to arrange.

## Shape of it

- **Its own window.** The button opens a separate browser window rather than a
  dialog, so it can be dragged to any size, moved to another screen or
  maximised. It remembers the size it was last dragged to and reopens that way.
- **Root shell.** The shell runs as root inside the account's own container,
  which is the same access the user already has over SSH. The container is
  always taken from the session: a client cannot name a container it does not
  own.
- **One account, two shells.** A third connection is offered the option to close
  the others instead of being refused, which is also how a slot held by a closed
  window is reclaimed.

## Surviving a bad connection

The panel is often reached from far away, so the browser side is built to
survive the link rather than to be torn down by it.

- **Liveness.** The server pings every 20 seconds and treats 60 seconds of
  silence as gone. The browser answers pings from its network stack, so this
  works in a backgrounded tab too, and a connection that disappears without
  saying so is noticed in about a minute.
- **Reconnect.** The page hears a small heartbeat the server sends alongside the
  protocol pings; hearing nothing for 45 seconds means the socket is dead even
  though it has not said so. It then reconnects with exponential backoff and
  jitter, up to eight attempts, after which a manual *Reconnect* is offered.
- **Resume.** The shell outlives the socket: when the client goes away the PTY
  keeps running for 120 seconds and its output goes into a bounded buffer
  (256 KiB, with a notice if anything is dropped). A client that comes back with
  its session token resumes the *same* shell — a running build, an open editor
  and the shell's variables are all still there.
- **Redraw.** A resumed terminal starts blank and is rebuilt from the buffered
  output, so a full-screen program is asked to repaint itself; if one does not,
  `Ctrl+L` forces it.
- **Token.** The resume token lives in `sessionStorage`, so reloading the page
  comes back to the shell and closing the window lets it go.

## Keys and the mouse

Typing works as it does in any terminal: arrows, Home/End, PageUp/PageDown,
Insert/Delete, Tab, Escape and the function keys all send what a terminal sends,
and the arrows follow the mode a full-screen program asks for.

A small bar above the terminal carries the few keys a browser keeps for itself
(Ctrl+C, Ctrl+Z, Ctrl+D, Escape, Tab and the arrows), so they are never out of
reach.

Copy and paste use the platform's own shortcuts — Cmd+C/Cmd+V, Ctrl+V,
Shift+Insert — and the browser's right-click menu. Pasting a block of several
lines is handed to the shell exactly as the shell asked for it: a shell that
enables bracketed paste shows the text in its edit buffer first.

## Turning it off

```
vps config set panel.web_ssh false
```

The panel restarts and the feature is off: the button is not rendered, and the
terminal page, its script and the terminal connection all refuse. `true` (the
default) turns it back on. A configuration written before this feature existed
comes out enabled.
