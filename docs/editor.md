# Connection editor

The form and YAML editors open in Vim-style normal mode. The shared browser
header remains visible while editing.

| Key                 | Action                                        |
| ------------------- | --------------------------------------------- |
| `j` / `k` or arrows | Move between fields or lines                  |
| `h` / `l`           | Move within text                              |
| `i` / `a`           | Enter insert mode                             |
| `ctrl+o`            | Return to normal mode                         |
| `x`                 | Delete a character                            |
| `u`                 | Undo                                          |
| `enter`             | Open field options                            |
| `ctrl+y`            | Open the syntax-highlighted YAML editor       |
| `o` / `dd`          | Add a line / delete a line in the YAML editor |
| `/`                 | Search YAML                                   |
| `?`                 | Show editor controls                          |

Use `:w` or `ctrl+s` to save. `:wq` saves and closes; `:q` closes an
unchanged draft; `:q!` discards unsaved edits. Press `esc` to return to the
parent view; if there are unsaved changes, the editor asks before discarding
them.