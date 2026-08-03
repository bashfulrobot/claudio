# claudio

Spawn, list, and delete [Claude Code](https://claude.com/claude-code) sessions from your phone.

No Anthropic API key, ever. Claudio is a thin wrapper around the `claude` CLI
itself — it shells out to the binary you already have logged in on your
machine, so every session it starts runs on your existing Claude Pro/Max
subscription, the same as if you'd typed `claude` in a terminal.

## What it does

- **Spawn** — start a full, persistent `claude --bg --remote-control` session
  in a folder of your choice. Not a one-shot `-p` call: a real background
  session you can keep talking to.
- **List** — see every session currently running, via `claude agents --json`.
- **Delete** — permanently remove a session with `claude rm <id>`.

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
| `-roots` | `~/git,~/dev` | Comma-separated project root dirs offered in the folder picker |

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/sessions` | List running sessions (`claude agents --json`) |
| `POST` | `/api/sessions` | Spawn `{"cwd": "...", "name": "..."}` |
| `DELETE` | `/api/sessions/{id}` | Delete a session by its `id` (`claude rm <id>`) |
| `GET` | `/api/folders` | List immediate subdirectories of the configured roots |

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
