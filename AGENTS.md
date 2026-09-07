# AGENTS.md

The working agreement for a coding agent in this repository. Identical in every
farcloser repository (content-pinned by `limen`); the reasoning behind each rule
is the [engineering book](https://github.com/farcloser/limen/tree/main/book).

## Workflows

- **Every workflow runs through `just`.** If a recipe covers the task, the recipe is the
  interface: `just do lint go`, `just do fix yaml`, `just do test go`, `just do tools set …`.
  Never invoke the underlying tools directly (no bare `golangci-lint`, `gofmt`, `go test`,
  `aqua`, `cargo`) when a recipe exists: recipes run the pinned versions on the hermetic
  PATH and often do more than the obvious command. Direct invocation is fine only when no
  recipe covers the need — say so when you do it. `just --list` shows every task.
- Always name the recipe; module defaults are curated subsets, not "run everything".
- Never bump a tool version by hand: `just do tools set` / `just do tools update`.

## Contributing

The doctrine is the book's
[coding agents as contributors](https://github.com/farcloser/limen/blob/main/book/agents.md)
chapter; the procedure is limen's `skills/contribute`.

- **Your own branch, in your own worktree**, cut from a fresh `main` and named after
  you, dated (`<bot>/<YYYYMMDD>-<topic>`), one topic per branch and per pull request. The human's
  branches — `work`, and anything not named after you — are the human's: never commit
  there unless pairing interactively at their request, and never on `main`.
- **Commits** are signed as you, with a DCO sign-off as you; when the change is the
  human's own work, the human is the author. No scratchpads (`AUDIT.md` and its kind).
- **Green before pushing:** the whole `just lint` and `just test`, not one lane.
- **Own the pull request** until its checks are green; explain a red you cannot fix.
- **Request the owner's review only then** — green, ready, and not stacked on an
  unmerged branch. The request is sent once; withdraw it if the pull request turns red.
- **Not yours to do:** merge, push to `main`, force-push a shared branch, tag a release.

## Scope

- **The ask is the deliverable** — thing A, whole; not B, not A plus B. Finish A, then
  *mention* the unrelated; acting on it is the human's call.
- **Drive-by fixes are fine; campaigns need a green light.** A pin, a stale suppression,
  a one-line workflow bug: on the way through. Onboarding a legacy repository, a
  wholesale cleanup of a broken one: the human decides first. Measure before moving.
- **A red inherited from `main`** is explained on the pull request, not fixed in it.
- **Doctrine can lose the argument, never silently.** A fix that cuts against the book is
  named as such and argued; it is decided, not discovered.

## Code

- **Pinned means by digest.** Every image, action, and tool — in code, examples, and
  documentation alike, because examples are what gets copied.
- **A linter finding is silenced by its rule, never by its linter:**
  `//revive:disable-next-line:<rule>`, `// #nosec G### -- reason`,
  `//nolint:staticcheck // SA####: reason`. Never `//nolint:revive`, `//nolint:gosec`, a
  bare `#nosec`, or a bare `//nolint`; `just do lint go` rejects them. See the book's
  [per-language rules](https://github.com/farcloser/limen/blob/main/book/per-language.md).
- **Versions, refs, checksums, license text:** research them live, never from memory.
