# tmp_agents scope: every top-level directory, Git custody by state

Status: design, not built. Ticket: TIN-7765. Issue: #171.

## Ruling

Jess, CADE-1 interview, November 2, 2026, verbatim selection
"Cleanup policy, 24h idle (Recommended)", recorded in Linear TIN-7765
comment `784131c9`:

> tinyland-cleanup removes `/srv/fast-local/tmp-agents` directories idle
> over 24 hours with no open files, and skips any git clone or worktree with
> unpushed or uncommitted work.

Context: sting's 128 GiB `/srv/fast-local/tmp-agents` project quota filled
twice on November 2. A seat removed 159 top-level directories, 56.7 GB, by
hand at 14:00Z (TIN-7765 comment `f2d60641`). The largest were agent work
directories with names such as `g55-s00r619` at 12.7 GB, not `nix-shell.*`
roots.

## What the plugin does at b00f102

The `retention` plugin's `tmp_agents` rule already has the 24 hour idle
test, the open-file test and the report-only mode the ruling needs. Two
parts of the ruling are out of its reach:

| Ruling part | b00f102 behaviour | Source |
|---|---|---|
| Any directory | Only names matching `name_globs` are inspected. The default is `nix-shell.*`. A bare `*` is refused at validation | `config/retention.go` `Validate`, `plugins/retention.go` `evaluateTmpAgents` |
| Idle 24h by newest change | Newest mtime or ctime of every entry, against `max_age` 24h | `plugins/retention.go` `walkTree`, `decide` |
| No open files | One process inventory: cwd, root, open fds, mappings, argv and `TMPDIR`, `TMP`, `TEMP` or `TEMPDIR`. Linux reads `/proc`, Darwin uses lsof. An incomplete inventory keeps everything | `plugins/bazel_bases_linux.go` `bbScanActivity` |
| Skip Git with unpushed or uncommitted work | Any `.git` inside the tree keeps it, clean or not. The one exception is a remote-less pytest fixture repository | `plugins/retention.go` `walkTree`, `retentionFixtureRepo` |
| Report first | `delete: false` reports what would go | `plugins/retention.go` `decide` |
| Live Claude scratchpads | `tmp-agents/claude-<uid>` is agent-session data and is never a candidate, live or not | `plugins/agentsession.go` `agentSessionUIDPatterns` |

So on sting the rule sees only `nix-shell.*` roots, and an agent work
directory that holds a clean, fully pushed clone stays forever. Most large
agent work directories hold clones: on November 2, 37 top-level
directories outside `claude-1000` held a `.git` entry within three levels.

## Measurement on sting, November 2, 14:38Z

A dry run of the pinned `b00f102` binary: `--once --dry-run --plugins
retention`, `delete: false`, `max_dirs_per_cycle: 0`. Sizes are `du`
megabytes read the same hour. Nothing was removed.

| Set | Trees | Size | Plugin decision |
|---|---|---|---|
| All of tmp-agents | | 70.6 GB | |
| `claude-1000`, 8 live Claude sessions | 8 | 29.5 GiB | denied, agent-session data |
| `claude-1000`, 78 ended sessions | 78 | 8.4 GiB | denied, agent-session data |
| Other top-level trees holding Git | 47 | 24.6 GiB | kept for `.git`. 46 changed within 24 h |
| Other top-level trees changed within 24 h | 158 | 4.5 GiB | kept as recent |
| Eligible | 0 | 0 | |

The run came 38 minutes after a hand removal of every tree idle over 4 h.
So today the 24 h rule frees nothing, and Git custody by state would free
one tree of 0.2 GiB. The value of this design is the steady state: Git
trees idle over 24 h stop accumulating. A refill of about 29 GB in 10 h
(TIN-7765 `f2d60641`) can still fill the quota within one 24 h window,
because live sessions and recent trees are kept by design.

## Proposal

### 1. `tmp_agents.all_names`

A new boolean, default `false`. When true, every top-level directory of
each root is a candidate and `name_globs` is not read. The bare `*`
refusal in `name_globs` stays. `all_names` is the explicit opt-in that
replaces it, so a host config states the intent in one word.

Guards that hold with `all_names`:

- Validation refuses `all_names` on a root that is `/`, the home directory,
  a parent of the home directory or a root of `dev_artifacts.protect_paths`.
- Agent-session names stay denied: `claude-*`, `codex*`, `.tmp*` and `pi-*`
  through `newAgentSessionDeny`.
- Files, symlinks, sockets and mount points stay out, as today.

### 2. `tmp_agents.git_custody`

A new string, default `any`, which is today's behaviour: any `.git` keeps
the tree. The new value `state` judges each repository by its state. A tree
goes only when every repository in it passes every check below. Anything
else keeps the tree, with the first failing check as the reason.

The walk records each repository it finds: a `.git` directory, a `.git`
file of a linked work tree, and submodules below them. More than
`max_repos_per_tree`, default 32, keeps the tree.

| Check | Command, run in the repository | Keeps the tree when |
|---|---|---|
| Uncommitted or untracked work | `git status --porcelain=v1 -z --untracked-files=normal --ignore-submodules=none` | output is not empty |
| Stash | `git rev-parse -q --verify refs/stash` | it succeeds |
| Commits on no remote branch | `git rev-list -n 1 --branches --tags HEAD --not --remotes`, then the same for each linked work tree's `HEAD` | it prints a commit |
| No remote to compare against | `git remote` and `git for-each-ref refs/remotes` | either is empty |
| Linked work tree whose main repository is outside the tree | `git rev-parse --git-common-dir` | the work tree is locked, or `git worktree remove` would refuse it |
| Main repository with linked work trees outside the tree | `.git/worktrees/*/gitdir` | any listed work tree still exists |

Remote-tracking refs are read as they are. The plugin never fetches. A
commit that was pushed but never fetched back reads as unpushed, so the
error is on the keeping side.

Every git call runs with `GIT_OPTIONAL_LOCKS=0`, `GIT_TERMINAL_PROMPT=0`,
`-c core.fsmonitor=false` and `-c core.hooksPath=/dev/null`, with a 10
second bound. A timeout, a non-zero exit with no defined meaning above, or
output that does not parse keeps the tree. Repositories of another user
are kept before any git call, as today.

A linked work tree whose main repository is outside the tree is removed
with `git -C <main> worktree remove <path>`, not `RemoveAll`. Git itself
refuses a dirty work tree there, which is a second check, and the main
repository's admin entry and branch lock go with it. A main repository
outside every retention root is never written in any other way.

The fixture rule from #170 is unchanged and applies under both values.

### 3. Claude scratchpads

Out of scope here. `tmp-agents/claude-<uid>` stays denied under R-W17
item 4, "no ageing of session data", and TIN-5268. If the operator wants
scratchpads of ended sessions reaped, issue #152 is the place. A session is
live when `~/.claude/sessions/<pid>.json` names a pid that runs and whose
`/proc/<pid>/stat` start time matches the file's `procStart`. That check
would run at session-directory level, `claude-<uid>/<project>/<session>`.

### 4. Report

Each target carries the repository count and the reason. Plan metadata
adds `tmp_agents.git_repos_judged`, `tmp_agents.kept_git_dirty`,
`tmp_agents.kept_git_stash`, `tmp_agents.kept_git_unpushed`,
`tmp_agents.kept_git_no_remote` and `tmp_agents.kept_git_worktree`.

## Tests

Each case builds a tree under a temp root with an mtime older than 24 h.

- `all_names` false: a non-matching name is not inspected.
- `all_names` true: an idle `agent-work-x` directory is eligible. A
  `claude-1000` directory is kept as agent-session data.
- `all_names` on the home directory fails validation.
- `git_custody: any`: a clean pushed clone keeps the tree.
- `git_custody: state`, each one kept with its reason: a modified tracked
  file, an untracked file, a stash, a local commit ahead of its remote
  branch, a branch with no remote counterpart, a local tag on an unpushed
  commit, a repository with no remote, a locked linked work tree and a main
  repository with an external linked work tree.
- `git_custody: state`: an ignored-only build directory inside a clean
  pushed clone does not keep the tree.
- `git_custody: state`: a clean linked work tree goes through
  `git worktree remove`, and its main repository's `worktree list` no
  longer names it.
- A git call that times out keeps the tree.
- The recheck before removal sees a file written after the plan and keeps
  the tree.

## Rollout

1. Build both options default off. Host configs that do not set them see
   no change.
2. Lab bumps the pin and sets `all_names: true` and `git_custody: state` on
   sting with `delete: false`. The daemon's report shows what would go.
3. A separate operator GO sets `delete: true`. Lab records it as a register
   amendment.

## Open questions

1. Commits that only a reflog still names, for example after a rebase that
   was pushed: this design does not count them, so they do not keep a tree.
   Should they?
2. Should `git worktree remove` on a main repository outside the root wait
   for its own ruling, with such work trees kept until then?
3. Should ended Claude sessions' scratchpads be reaped under #152?
