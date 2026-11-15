# Usage

Run `bastionctl` in a terminal to start interactive discovery or open the
saved-connection browser. For non-interactive use, the CLI prints the result
and exits.

## Commands

```bash
bastionctl                          # interactive discovery or browser
bastionctl --aws-profile prod       # use an AWS shared-config profile
bastionctl --connection corp        # use or create a named connection
bastionctl --vpc-id vpc-0abc123     # target a specific VPC
bastionctl --region eu-central-1    # override region
bastionctl --rescan corp            # refresh a saved connection
bastionctl --forget corp            # delete a saved connection
bastionctl --launch                 # run sshuttle in the foreground
bastionctl --start corp             # start sshuttle in the background
bastionctl --stop corp              # stop a background tunnel
bastionctl --status corp            # show tunnel status
bastionctl --logs                   # follow daemon logs
bastionctl --version                # print version and project URL
```

Split DNS options are described in the [Split DNS guide](split-dns.md).

```bash
bastionctl --dns split --dns-domain corp.internal --dns-resolver 10.0.0.2 corp
bastionctl corp --manage-dns --start
bastionctl --dns-setup corp
bastionctl --dns-teardown corp
```

The hidden `--demo` flag opens the browser with simulated data. It makes no
AWS calls and changes no local files. See the [browser guide](interactive-browser.md).

`--start` uses sshuttle's native daemon mode and may prompt for sudo. `--logs`
streams system log history: system-wide logs on macOS, journald or
`/var/log/syslog` on Linux. `--stop` validates the PID file before sending
SIGTERM.