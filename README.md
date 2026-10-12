# claudio

Spawn, list, and delete [Claude Code](https://claude.com/claude-code) sessions from your phone.

No Anthropic API key, ever. Claudio is a thin wrapper around the `claude` CLI
itself — it shells out to the binary you already have logged in on your
machine, so every session it starts runs on your existing Claude Pro/Max
subscription, the same as if you'd typed `claude` in a terminal.

## What it does

- **Spawn** — start a full, persistent `claude --bg --remote-control` session
  in a folder of your choice. Not a one-shot `-p` call: a real background
  session you can keep talking to. Optionally set a model, effort level,
  permission mode, or an initial prompt — leave any of them blank and
  `claude` falls through to your own `~/.claude/settings.json`, exactly as
  if claudio weren't in the way.
- **Worktree-isolated by default** — every new session gets its own git
  worktree (`claude --worktree`), so two sessions spawned in the same repo
  never stomp on each other's uncommitted work. Skipped automatically for
  non-git folders or a folder that's already a worktree; can be turned off
  per spawn.
- **List** — see every session currently running, via `claude agents --json`.
- **Stop** — pause a session while keeping its conversation, via
  `claude stop <id>`. Resume it later from a terminal with
  `claude attach <id>`. Unlike Delete, this leaves the worktree in place.
- **Delete** — permanently remove a session with `claude rm <id>`, which
  also removes its worktree if it has one.

Once a session is spawned with `--remote-control`, it's also visible and
controllable from Anthropic's own [claude.ai/code](https://claude.ai/code)
and the iOS/Android apps — claudio only handles the part those don't:
kicking off new sessions and tearing down old ones from a phone.

## Why not just kill the process?

Background sessions run under a `claude bg-pty-host` supervisor that
transparently respawns a killed worker (new pid, same session) within
seconds. `claude rm <id>` is the real, sanctioned way to end a session for
good — the same primitive the interactive agent view's `Ctrl+X` twice
binding uses under the hood.

## Running it

```bash
go build -o claudio .
./claudio -addr 127.0.0.1:8787 -roots ~/git,~/dev
```

Or via the flake:

```bash
nix run github:bashfulrobot/claudio -- -addr 127.0.0.1:8787
```

Then open `http://127.0.0.1:8787` — the frontend is a single embedded page,
laid out for a phone screen. Bind `-addr` to a Tailscale IP (or put it behind
`tailscale serve`) rather than `0.0.0.0` if you want it reachable from your
phone without exposing it to the whole LAN.

### Flags

| Flag | Default | Description |
|---|---|---|
| `-addr` | `127.0.0.1:8787` | Listen address |
| `-claude-bin` | `claude` | Path to the `claude` CLI binary |
| `-git-bin` | `git` | Path to the `git` binary, used to decide whether a new session can be worktree-isolated |
| `-roots` | `~/git,~/dev` | Comma-separated project root dirs offered in the folder picker |

### Workspace trust

`claude --bg` has no terminal for the workspace trust dialog, so an untrusted folder fails with `Workspace not trusted`. Before each spawn, claudio sets `projects["<path>"].hasTrustDialogAccepted` to `true` in `~/.claude.json` (or `$CLAUDE_CONFIG_DIR/.claude.json`). The path is the git repo root (the main checkout for a worktree) or the folder itself outside a repo. Spawns are refused for folders outside `-roots`, and for the home directory, where claude never saves trust. Trusting a repo activates its hooks and allow rules, so only point `-roots` at repos you wrote or reviewed.

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/sessions` | List all sessions (`claude agents --json --all`); `disconnected: true` marks ones with no process |
| `POST` | `/api/sessions` | Spawn a session — see body below |
| `POST` | `/api/sessions/{id}/stop` | Stop a session, keeping its conversation (`claude stop <id>`) |
| `POST` | `/api/sessions/{id}/resume` | Restart a disconnected session under the same id (`claude respawn <id>`) |
| `DELETE` | `/api/sessions/{id}` | Permanently delete a session and its worktree (`claude rm <id>`) |
| `POST` | `/api/sessions/bulk` | `{"action":"remove"\|"resume","ids":[…]}` (max 50); returns one result per id, so a refused delete does not hide the others |
| `GET` | `/api/folders` | List immediate subdirectories of the configured roots |

`POST /api/sessions` body:

```json
{
  "cwd": "/home/you/git/some-project",
  "name": "optional session name",
  "worktree": true,
  "model": "opus",
  "effort": "high",
  "permissionMode": "acceptEdits",
  "prompt": "optional first message"
}
```

Only `cwd` is required. `worktree` defaults to `true` when omitted — every
new session is isolated into its own git worktree unless you explicitly set
it to `false`, the target folder isn't a git repo, or it's already a
worktree. `model`, `effort`, `permissionMode`, and `prompt` are all optional;
leaving any of them out omits the matching `claude` flag entirely rather than
sending some default value, so `~/.claude/settings.json` decides.

The response reports what actually happened:

```json
{ "name": "some-project", "worktree": "some-project" }
```

or, when isolation was skipped:

```json
{ "name": "some-project", "worktreeSkipped": "not a git repo" }
```

## NixOS

The flake exposes `nixosModules.default` (`services.claudio`) for running
this as a systemd service:

```nix
{
  inputs.claudio.url = "github:bashfulrobot/claudio";

  outputs = { claudio, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      modules = [
        claudio.nixosModules.default
        {
          services.claudio = {
            enable = true;
            user = "you"; # must be the user that ran `claude login`
            listenAddr = "127.0.0.1:8787";
            roots = [ "/home/you/git" ];
          };
        }
      ];
    };
  };
}
```

`services.claudio` runs as the given user rather than a sandboxed
`DynamicUser`, because `claude`'s subscription auth lives under that user's
`~/.claude` — claudio itself never touches credentials.

## License

MIT — see [LICENSE](LICENSE).
