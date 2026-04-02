# Command Filter Rule Language

A small language for defining allowed command invocations. Each rule file
describes the permitted argument shapes for a command, with path arguments
carrying read/write permission annotations.

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
be wrapped in angle brackets and must not collide with built-in types
(`string`, `path`).

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

| Placeholder    | Matches                              |
|----------------|--------------------------------------|
| `<string>`     | Any argument                         |
| `<string:->`   | Any argument (dash-allowed modifier) |
| `<path:r>`     | A path the user can read             |
| `<path:w>`     | A path the user can write            |
| `<name>`       | Expands a `define`d sub-pattern      |

User-defined names are distinguished from built-in types by their presence in
a `define` statement.

### Groups and alternatives

Parentheses group elements. Pipe separates alternatives within a group:

```
(-g | -F)
(-name <string:-> | -type <string:->)
```

A group matches exactly one of its alternatives.

### Optional

Square brackets mark an element or group as optional (zero or one):

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

## Future extensions

The keyword-prefixed design reserves space for future statement types (e.g.
`deny`, `#` comments) without ambiguity.
