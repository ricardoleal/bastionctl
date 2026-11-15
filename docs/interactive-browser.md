# Interactive browser

Run `./bastionctl` in a terminal to open the full-screen connection browser.
It has a home list of saved connections and a detail view for the selected
connection. The shared header shows account and connection details, while
contextual key hints and notices stay visible as you work.

## Navigation

| Key                 | Action                                                                       |
| ------------------- | ---------------------------------------------------------------------------- |
| `j` / `k` or arrows | Move through the current list                                                |
| `enter`             | Open the highlighted connection                                              |
| `/`                 | Search the current view; `enter` applies the filter                          |
| `esc`               | Clear search, or return to the previous view                                 |
| `:`                 | Enter a command such as `stop corp`, `dns install corp`, `rescan`, or `help` |
| `tab`               | Accept an inline command or connection suggestion                            |
| `?`                 | Show available controls                                                      |

Other actions, such as starting, stopping, editing, and forgetting a
connection, run on the highlighted row. The header shows the available keys.
Long lists scroll inside the terminal window; use `PgUp` and `PgDn` to move by
page. Space toggles a CIDR in the network selector.

## Connection states

The home list includes cached connections from every account. Status markers
indicate running, idle, unreachable, and split-DNS resolver-missing states.
Open a connection to see tunnel status, target reachability, DNS state,
selected routes, and the generated `sshuttle` command.

The browser works offline with cached connections. Actions that only need the
saved configuration remain available; creating or rescanning a connection
requires AWS access. Notifications appear over the panel and clear after a
few seconds or on the next key press.

The GitHub footer uses a Nerd Font glyph when supported. In iTerm it defaults
to plain text. Set `BASTIONCTL_ICONS=1` to enable the glyph with a Nerd Font,
or `BASTIONCTL_NO_ICONS=1` to force plain text in any terminal.

## Demo mode

Run `./bastionctl --demo` to explore four simulated connections across two
sample accounts. Actions update mock state and display sample output, but make
no AWS calls and do not touch config, pid, or resolver files. The flag is
hidden from `--help`.

CLI flags remain available for scripting. Non-terminal runs keep the
single-shot output behavior.