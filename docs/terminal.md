# Terminal Contract

Parrot uses the terminal's normal scrollback. It is not a screen application,
but it may redraw the last few lines that it explicitly owns.

## Forbidden Behavior

Interactive commands must not emit:

- alternate-screen enter or leave sequences;
- absolute cursor positioning;
- clear-screen sequences;
- mouse tracking;
- terminal-title changes;
- resize-triggered transcript replay;
- mutable regions outside the renderer-owned bottom region.

The terminal renderer owns two bounded logical regions at the bottom of the
terminal. The live status/response region may use at most six rows. Beneath it,
the input region normally occupies one logical row and may expand to at most
twelve rows while a picker, completion menu, permission, or question dialog is
open. One renderer redraws both regions atomically; they are not independent
cursor owners. Only this combined bottom region may use carriage return,
relative cursor up/down, erase-line, and temporary cursor hide/show sequences.
Clearing or redrawing it must never alter committed transcript above it.
Alternate screens, full-screen clears, mouse tracking, title changes, and
replaying the committed transcript remain forbidden.

Color is optional for human-oriented interactive output and is disabled by
`NO_COLOR`, `--no-color`, `TERM=dumb`, or non-TTY output. Untrusted model and
tool text is sanitized before renderer-owned styling is applied.

Assistant responses render a terminal-safe Markdown subset: headings,
blockquotes, ordered and unordered lists, task boxes, thematic rules, inline
code, emphasis, strong text, strikethrough, and links. Fenced code delimiters
are hidden. Known fence languages use embedded Chroma lexers for syntax
highlighting on a color-capable TTY; unknown or explicitly plain languages,
disabled color, and non-TTY output use plain code. Highlighting falls back to
plain code above 512 KiB or 10,000 lines. Model content is sanitized before
Markdown parsing, and only renderer-owned presentation metadata may introduce
ANSI sequences.

## Chat Loop

Enhanced chat keeps one editor active while the session is idle or running:

```text
edit -> submit steer -> continue editing while events stream
```

The editor supports rune-aware movement, history, slash completion, bracketed
paste, Ctrl-J multiline input, and the conventional Ctrl-A/Ctrl-E bindings for
moving to the beginning/end of the current line. Ctrl-K clears from the cursor
to the end of the current line. Enter starts a turn while idle
and steers the active turn at the next safe provider-turn boundary while the
agent is running. Safe informational slash commands execute
immediately; commands that switch or mutate the active session are rejected
until it is idle. Stable assistant rows are progressively committed to normal scrollback as they
arrive; only the unfinished final row, pending-input previews, the working
spinner, and transient tool/status activity may redraw in the bounded renderer
region. Each assistant response and divider is committed exactly once. Permission
and question prompts temporarily take keyboard focus in the same renderer; the
ordinary message draft is preserved and the surrounding transcript remains
immutable. Picker and dialog choices use the expandable input region rather
than consuming the six live-status rows. A moving viewport keeps the selected
choice visible. When not every choice fits, a written `Showing … of … options;
… hidden` row reports the omitted choices. Enter submits a selected question
option immediately. Questions that also accept free-form answers show a
separate `Custom input` row; choosing it switches the question prompt to text
editing, where Enter submits the typed answer. For multiple-choice questions,
Space stages additional options and Enter submits the staged options together
with the highlighted one.

Shift-Tab switches modes; Ctrl-X is the portable fallback for terminals that do
not send the Shift-Tab sequence. Unbound control keys and malformed keyboard
input are ignored and cannot terminate the chat session.

Committed turns use aligned role markers and hanging indentation:

```text
$ User message
  continuation line
- Assistant response
  continuation line
───────────────────────────────────────
⠋ $ Editable while the agent works
```

The `$` marker is the user accent. User messages use a dark-blue background with
pale-yellow text. The `-` marker is the assistant accent, and assistant messages
use a dark-green background with pale-cyan text. Neither message has a leading
divider. The divider after an assistant response is the input area's top border.
Pending follow-ups appear below it with a written
`(○ pending)` label.
Human-oriented state uses an icon plus a written label where practical: the
Braille spinner means working, `✓ Done` means success, `✗ Failed` or `✗ Error`
means failure, `■ Interrupted` means execution stopped, and `○ Pending` means
pending. Status text is dim and errors are red when color is enabled. The words
remain present without color, so meaning never depends on styling or symbols.

Permanent transcript output has compact and block entries. Consecutive one-line
tool statuses are compact and have no padding between them. Assistant responses
and successful `edit` output are blocks. Role colors distinguish user and
assistant messages without a leading rule; other block boundaries retain one
empty line. An edit block contains its completion status followed immediately
by the first 10 lines of the reviewed unified before/after diff; no empty line
splits that status and diff. Failed tool request blocks use the same 10-line limit.
Longer blocks end with the number of omitted lines.

### Flush boundaries

The live region and permanent scrollback have explicit ownership boundaries:

1. Reasoning summaries remain redrawable while they are being updated. At the
   first nonempty answer-text delta, they are committed in provider order before
   any answer text can enter scrollback. Raw chain-of-thought is never committed.
2. During an answer, each complete source line is promoted to scrollback.
   Width-wrapped fragments remain redrawable until their source line is
   complete, so later input cannot reinterpret already-committed Markdown.
   Fenced code remains redrawable until the closing fence, preserving
   multiline lexer state. The live preview remains bounded to six rows.
3. Completed tool reports are queued while an assistant message is open. After
   the answer suffix is committed, reports are flushed in completion order. This
   prevents a tool status from splitting the assistant response.

Thus the permanent order for a turn is reasoning summaries, assistant answer,
then any tool reports that completed while that answer was open.

## Structured Data Presentation

Human-facing terminal output must display structured JSON data as block-style
YAML for readability. This includes tool requests and failed-request details.
Permission dialogs are an exception: they render only the tool's human-readable
description, flattened to one line. Policy metadata, resource records,
canonical input, and structured review data remain internal authorization data
and are not rendered. JSON remains the format for APIs, JSONL output,
configuration examples, and other explicitly machine-readable interfaces.

Ctrl-C clears a nonempty draft. With an empty busy editor, the first Ctrl-C
requests cancellation and remains in the session; a second interrupt before
cancellation completes exits with status 130. Ctrl-D exits only from an empty
idle prompt.

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

Status and decisions are expressed with words even when an icon is also used;
meaning never depends on color or symbols alone.
Permission scope and default choices are written before input is requested.
The working spinner is confined to one renderer-owned cell and never enters the
committed transcript. There are no repeated transcript blocks. Tables have
machine-readable alternatives and remain understandable when wrapped.
