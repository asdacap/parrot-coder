# Terminal Contract

Parrot uses the terminal's normal scrollback. It is not a screen application.

## Forbidden Behavior

Interactive commands must not emit:

- alternate-screen enter or leave sequences;
- cursor positioning;
- clear-screen or erase-line sequences;
- carriage-return spinners;
- mouse tracking;
- terminal-title changes;
- resize-triggered transcript replay;
- mutable footer or status regions.

Color is optional on terminal stderr and is disabled by `NO_COLOR`,
`--no-color`, or `TERM=dumb`.

## Chat Loop

The first implementation reads ordinary user input only while the session is
idle:

```text
read line -> submit -> stream until idle -> read next line
```

This avoids redrawing a partially edited prompt while asynchronous output is
arriving. Assistant deltas append immediately. Tool, permission, question, and
status changes are immutable lines.

During a turn, the first `Ctrl-C` requests cancellation and remains in the
session. A second interrupt before cancellation completes exits with status
130. `Ctrl-D` on an empty idle prompt exits cleanly.

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
