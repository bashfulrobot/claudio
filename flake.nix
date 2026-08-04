{
  description = "Spawn, list, and delete Claude Code sessions from a phone";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forEachSystem = nixpkgs.lib.genAttrs systems;
      pkgsFor = system: nixpkgs.legacyPackages.${system};
    in
    {
      packages = forEachSystem (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = pkgs.buildGoModule {
            pname = "claudio";
            version = "0.1.0";
            src = self;
            # go.mod declares no external dependencies (stdlib only).
            vendorHash = null;
            meta = with pkgs.lib; {
              description = "Spawn, list, and delete Claude Code sessions from a phone";
              homepage = "https://github.com/bashfulrobot/claudio";
              license = licenses.mit;
              mainProgram = "claudio";
            };
          };
        }
      );

      devShells = forEachSystem (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
            ];
          };
        }
      );

      nixosModules.default =
        {
          config,
          lib,
          pkgs,
          ...
        }:
        let
          cfg = config.services.claudio;
        in
        {
          options.services.claudio = {
            enable = lib.mkEnableOption "claudio, a phone-friendly Claude Code session manager";

            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.system}.default;
              description = "The claudio package to run.";
            };

            user = lib.mkOption {
              type = lib.types.str;
              description = ''
                User to run claudio as. This must be the same user that has
                run `claude login`, since claudio shells out to the local
                `claude` CLI and relies on its existing subscription auth —
                claudio itself never handles API keys or credentials.
              '';
            };

            listenAddr = lib.mkOption {
              type = lib.types.str;
              default = "127.0.0.1:8787";
              description = ''
                Address to bind to. Set this to a Tailscale IP (not
                0.0.0.0) to expose claudio only on the tailnet.
              '';
            };

            roots = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              description = "Project root directories to offer in the folder picker.";
            };

            claudeBinary = lib.mkOption {
              type = lib.types.str;
              default = "claude";
              description = "Path to (or name of) the claude CLI binary.";
            };

            gitBinary = lib.mkOption {
              type = lib.types.str;
              default = "git";
              description = ''
                Path to (or name of) the git binary, used to decide whether a
                newly spawned session can be isolated into a git worktree.
              '';
            };
          };

          config = lib.mkIf cfg.enable {
            systemd.services.claudio = {
              description = "claudio session manager";
              wantedBy = [ "multi-user.target" ];
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];
              # The default systemd unit PATH covers coreutils and friends
              # but NOT git -- without this, worktree-by-default silently
              # degrades to never-worktree under the module while working
              # fine in a dev shell, since exec.Command("git", ...) just
              # fails to find the binary.
              path = [ pkgs.git ];
              serviceConfig = {
                ExecStart =
                  "${cfg.package}/bin/claudio"
                  + " -addr ${cfg.listenAddr}"
                  + " -claude-bin ${cfg.claudeBinary}"
                  + " -git-bin ${cfg.gitBinary}"
                  + lib.optionalString (cfg.roots != [ ]) " -roots ${lib.concatStringsSep "," cfg.roots}";
                User = cfg.user;
                Restart = "on-failure";
                RestartSec = 5;
                # claude's own subscription auth lives under the user's
                # ~/.claude, so this needs a real login shell environment,
                # not a stripped-down DynamicUser sandbox.
              };
            };
          };
        };
    };
}
