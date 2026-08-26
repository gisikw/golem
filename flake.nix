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
            wrapProgram $out/bin/golemd --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.tmux pkgs.git pkgs.bash ]}
          '';
        });
        familiar-render = (build "golem-familiar-render" [ "./cmd/golem-familiar-render" ]).overrideAttrs (old: {
          postInstall = (old.postInstall or "") + ''
            wrapProgram $out/bin/golem-familiar-render --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.tmux ]}
          '';
        });
      in {
        packages = { inherit cli daemon familiar-render; default = cli; };
        apps = {
          default = { type = "app"; program = "${cli}/bin/golem"; meta.description = "Control Golem delegated agents"; };
          golem = { type = "app"; program = "${cli}/bin/golem"; meta.description = "Control Golem delegated agents"; };
          golemd = { type = "app"; program = "${daemon}/bin/golemd"; };
          golem-familiar-render = { type = "app"; program = "${familiar-render}/bin/golem-familiar-render"; };
        };
        checks = {
          inherit cli daemon familiar-render;
          agent-hooks = pkgs.runCommand "golem-agent-hooks-tests" { nativeBuildInputs = [ pkgs.bun ]; } ''
            cp -r ${./integrations/pi/agent-hooks} ./agent-hooks
            chmod -R u+w ./agent-hooks
            bun test ./agent-hooks/events.test.ts
            touch $out
          '';
          familiar-extension = pkgs.runCommand "golem-familiar-extension-tests" { nativeBuildInputs = [ pkgs.bun ]; } ''
            mkdir -p ./contrib
            cp -r ${./contrib/familiar} ./contrib/familiar
            cp ${./flake.nix} ./flake.nix
            chmod -R u+w ./contrib/familiar
            bun test ./contrib/familiar/pi/agents/resolve.test.ts ./contrib/familiar/pi/agents/cli.test.ts ./contrib/familiar/pi/agents/extension.test.ts ./contrib/familiar/manifest.test.ts
            touch $out
          '';
        };
        devShells.default = pkgs.mkShell {
          # pi is pinned here deliberately: workers resolve `pi` from PATH, and an
          # ambient stale install (e.g. an old npm global) makes hook events
          # silently vanish and jobs never settle. Other harness CLIs (claude,
          # codex) intentionally fall through to the system.
          packages = [ pkgs.go pkgs.tmux pkgs.git pkgs.bun pkgs.bashInteractive pkgs.pi-coding-agent ];
          GOLEM_INTERACTIVE_SHELL = "${pkgs.bashInteractive}/bin/bash";
        };
      });
}
