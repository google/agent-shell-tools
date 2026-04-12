# Command Filter Rule Language

A small language for defining allowed command invocations. Each rule file
describes the permitted argument shapes for a command, with path arguments
carrying read/write permission annotations.

See [README.md](README.md) for the security model and practical guidance.

## Statements

Every line is one of two statement types, identified by a keyword prefix:

```
allow <pattern>
define <name> <pattern>
```

Blank lines are ignored. Order of statements is unrestricted, but the
recommended style is `allow` first (like a man page synopsis), then `define`:

```
allow rg [<options>]... <string> <path:r>...
allow rg --help
allow rg --version

define <options> (-g | -F)
```

### `allow`

Declares a permitted invocation pattern. The first token in the pattern is the
command name (matched literally). Multiple `allow` lines for the same command
act as alternatives — a command is permitted if it matches any one of them.

### `define`

Binds a name to a sub-pattern that can be referenced elsewhere. The name must
be wrapped in angle brackets and must not collide with the built-in type names
`string` or `path`.

Definitions may reference other definitions. The reference graph must be
acyclic (no recursion).

## Patterns

A pattern is a sequence of elements:

### Literals

Bare tokens match exactly:

```
allow rg --help
```

### Placeholders

Angle-bracketed names that match a single argument:

| Placeholder    | Matches                                          |
|----------------|--------------------------------------------------|
| `<string>`     | Any argument that does not start with `-`        |
| `<string:->`   | Any argument, including those starting with `-`  |
| `<path:r>`     | A path the user can read                         |
| `<path:w>`     | A path the user can write                        |
| `<name>`       | Expands a `define`d sub-pattern                  |

Modifiers (the `:` suffix, e.g. `:-`, `:r`, `:w`) are only valid on built-in
types. User-defined names must be plain `<name>` with no modifier.

User-defined names are distinguished from built-in types by their presence in
a `define` statement.

### Prefixed placeholders

A literal prefix immediately followed by a placeholder (no separating space)
forms a single element that matches one argument:

```
--color=<string>      # matches --color=auto, --color=never, …
--output=<path:w>     # matches --output=/tmp/out (path must be writable)
/LOG:<path:w>         # matches /LOG:build.log (any literal prefix works)
```

The argument must start with the literal prefix, and the remainder (everything
after the prefix) is validated by the placeholder.

The placeholder's normal matching rules apply to the remainder. For example,
`--flag=<string>` rejects `--flag=-x` because `<string>` does not accept
arguments starting with `-`, while `--flag=<string:->` accepts it.

User-defined names are also valid after a prefix, provided every path through
the definition consumes exactly one argument:

```
define <level> (debug | info | warn | error)

allow logger --level=<level>       # ok: <level> is single-argument
```

A definition whose expansion contains multi-element sequences, repetition, or
optional groups is not single-argument and is rejected in prefix position at
parse time.

### Groups and alternatives

Parentheses `()` and square brackets `[]` both group elements. Pipe `|`
separates alternatives within the closest enclosing group:

```
(-g | -F)
(-name <string:-> | -type <string:->)
[-l | -t | -h]
```

A group matches exactly one of its alternatives. Each alternative may be a
sequence of multiple elements, where each element consumes one argument:

```
(-name <string:-> | -type <string:->)   # each branch is two arguments
```

### Optional

Square brackets mark a group as optional (zero or one):

```
[<string>]
[-l | -t | -h]
```

### Repetition

An ellipsis `...` after an element means one or more:

```
<string>...       # one or more strings
<path:r>...       # one or more readable paths
```

### Combining optional and repetition

`[X]...` means zero or more — the optionality of `[]` composes with the
repetition of `...`:

```
[<options>]...    # zero or more options
[-r | -f]...     # zero or more of these flags
```

## Ambiguity

Patterns must be **1-unambiguous**: at every point where matching could follow
two paths (continue a repetition vs. advance to the next element, or choose
between alternatives), the sets of arguments each path accepts must be
disjoint. The matcher decides where each argument belongs by inspecting that
argument alone, with no lookahead or backtracking.

A pattern that violates this property is rejected at parse time:

```
<path:r>... <path:w>          # error: both sides accept paths
[<string>]... <string>        # error: same type on both sides
[<string:->]... <string>      # error: <string:-> is a superset of <string>
```

Valid patterns keep choice points disjoint:

```
[-v | -r]... <string>         # ok: literals are disjoint from <string>
[<options>]... <string>       # ok: options expand to dash-prefixed flags
```

## Future extensions

The keyword-prefixed design reserves space for future statement types (e.g.
`deny`, `#` comments) without ambiguity.
