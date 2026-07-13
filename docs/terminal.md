# Terminal Contract

Parrot uses the terminal's normal scrollback. It is not a screen application.

## Forbidden Behavior

Interactive commands must not emit:

- alternate-screen enter or leave sequences;
- absolute cursor positioning;
- clear-screen sequences;
- mouse tracking;
- terminal-title changes;
- resize-triggered transcript replay;
- mutable regions outside the renderer-owned bottom region.

The terminal renderer may own a bounded region of at most six rows at the
bottom of the terminal. Only that region may use carriage return, relative
cursor up/down, erase-line, and temporary cursor hide/show sequences. Clearing
or redrawing it must never alter committed transcript above it. Alternate
screens, full-screen clears, mouse tracking, title changes, and replaying the
committed transcript remain forbidden.

Color is optional on terminal stderr and is disabled by `NO_COLOR`,
`--no-color`, or `TERM=dumb`.

## Chat Loop

Enhanced chat reads ordinary user input only while the session is idle:

```text
read line -> submit -> stream until idle -> read next line
```

The editor supports rune-aware movement, history, slash completion, bracketed
paste, and Ctrl-J multiline input. Assistant text and transient tool/status
activity may redraw only in the bounded renderer region. Once a turn is idle,
the complete assistant response is committed exactly once. Permission and
question prompts temporarily take ownership of the same renderer; decisions
and the surrounding transcript remain immutable.

During a turn, the first `Ctrl-C` requests cancellation and remains in the
session. A second interrupt before cancellation completes exits with status
130. `Ctrl-D` on an empty idle prompt exits cleanly.

If raw mode cannot be enabled, or input/output are not real terminals, explicit
`parrot chat` uses a deterministic line REPL with no terminal escape sequences.

## Output Channels

`parrot run --format text` writes only assistant text to stdout. Status, tool
activity, warnings, and errors go to stderr. Stdout never contains ANSI escape
sequences and ends with one newline after a successful textual response.

`parrot run --format jsonl` writes one typed event per stdout line. Human
decorations are disabled. Fatal startup and usage failures use stderr.

Piped stdin is prompt data only. It can never answer a permission or question.
Interactive replies with piped input require an explicit option and a controlling
terminal.

## Accessibility

Status and decisions are expressed in words rather than color or symbols alone.
Permission scope and default choices are written before input is requested.
There are no rapidly changing spinners or repeated transcript blocks. Tables
have machine-readable alternatives and remain understandable when wrapped.
