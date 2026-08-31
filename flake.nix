{
  description = "Standalone Golem delegated-agent system";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };
  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        build = name: subPackages: pkgs.buildGoModule {
          pname = name;
          version = "0.1.0";
          src = ./.;
          inherit subPackages;
          vendorHash = "sha256-oeJJeFerfb5gT+NE3eTk85zguwqbIOvD2Y7CYOrCAVg=";
          doCheck = true;
          nativeBuildInputs = [ pkgs.makeWrapper ];
          meta = with pkgs.lib; { license = licenses.mit; platforms = platforms.unix; };
        };
        cli = build "golem" [ "./cmd/golem" ];
        daemon = (build "golemd" [ "./cmd/golemd" ]).overrideAttrs (old: {
          postInstall = (old.postInstall or "") + ''
            wrapProgram $out/bin/golemd --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.tmux pkgs.git pkgs.bash pkgs.nix pkgs.claude-code ]}
          '';
        });
        # Combined output for deployment: one profile carrying every Golem
        # binary, so trackers (e.g. fort-nix tracked services) can build a
        # single attr without CLI/daemon version skew.
        full = pkgs.symlinkJoin {
          name = "golem-full";
          paths = [ cli daemon ];
        };
      in {
        packages = { inherit cli daemon full; default = cli; };
        apps = {
          default = { type = "app"; program = "${cli}/bin/golem"; meta.description = "Control Golem delegated agents"; };
          golem = { type = "app"; program = "${cli}/bin/golem"; meta.description = "Control Golem delegated agents"; };
          golemd = { type = "app"; program = "${daemon}/bin/golemd"; };
        };
        checks = {
          inherit cli daemon;
          agent-hooks = pkgs.runCommand "golem-agent-hooks-tests" { nativeBuildInputs = [ pkgs.bun ]; } ''
            cp -r ${./integrations/pi/agent-hooks} ./agent-hooks
            chmod -R u+w ./agent-hooks
            bun test ./agent-hooks/events.test.ts
            touch $out
          '';
          tiamat-extension = pkgs.runCommand "golem-tiamat-extension-tests" { nativeBuildInputs = [ pkgs.bun ]; } ''
            cp -r ${./integrations/pi/tiamat} ./tiamat
            chmod -R u+w ./tiamat
            bun test ./tiamat/catalog.test.ts
            touch $out
          '';
        };
        devShells.default = pkgs.mkShell {
          # pi is pinned here deliberately: workers resolve `pi` from PATH, and an
          # ambient stale install (e.g. an old npm global) makes hook events
          # silently vanish and jobs never settle. Other harness CLIs (claude,
          # codex) intentionally fall through to the system. Claude Code is supplied
          # from nixpkgs (and therefore pinned by flake.lock), rather than an
          # ambient npm global.
          packages = [ pkgs.go pkgs.tmux pkgs.git pkgs.bun pkgs.bashInteractive pkgs.pi-coding-agent pkgs.claude-code ];
          GOLEM_INTERACTIVE_SHELL = "${pkgs.bashInteractive}/bin/bash";
        };
      });
}
