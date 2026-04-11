# Command Filter

A command filter evaluates a proposed command line against a developer-authored
rule file and returns allow or deny. It is used to narrowly delegate specific
CLI capabilities to an agent, including commands that intentionally use the
user's ambient credentials.

See [LANGUAGE.md](LANGUAGE.md) for the rule language specification.

## Security model

Command filtering complements sandboxing by narrowing which command lines an
agent may run: which binaries are allowed, which subcommands and flags are
allowed, and which logical paths may be passed as arguments. That is a
least-authority delegation mechanism, not a full security boundary. Most agent
actions should run under [`//sandbox`](../sandbox/BUILD.bazel); command
filtering is not a substitute for sandboxing.

If an allowed command can use the user's ambient credentials, network access,
or other external authority, then allowing that command is effectively
granting that capability to the agent. Path restrictions such as `<path:r>`
and `<path:w>` only constrain named file arguments; they do not constrain
unrelated side effects a command may have.

For example, a user might grant an agent permission to fetch GitHub Actions
logs via a narrow `gh` rule. That is different from allowing arbitrary `gh`
usage: the rule is a deliberate delegation of one credential-backed
capability, not blanket authority over the GitHub CLI.

Some allowed tools may execute hooks, helpers, pagers, editors, or other
user-controlled programs as part of their normal behavior.
Rules should be written with those secondary execution paths in mind.

## Practical guidance

- Prefer running agent actions in `//sandbox`.
- Use command filters to grant narrow, reviewable capabilities, especially for
  tools that intentionally act with the user's ambient credentials.
- Write rules per operation, not per binary.
- Treat each allow-rule as a permission grant that should be reviewed.
