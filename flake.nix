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
          vendorHash = "sha256-dsmRXd5moOA08U2Hbi9Z3Hy1inZFiDOD9AMS56uk+8g=";
          doCheck = true;
          nativeBuildInputs = [ pkgs.makeWrapper ];
          meta = with pkgs.lib; { license = licenses.mit; platforms = platforms.unix; };
        };
        cli = build "golem" [ "./cmd/golem" ];
        service = build "golem-service" [ "./cmd/golem-service" ];
        supervisor = (build "golem-supervisor" [ "./cmd/golem-supervisor" ]).overrideAttrs (old: {
          postInstall = (old.postInstall or "") + ''
            wrapProgram $out/bin/golem-supervisor --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.tmux pkgs.git pkgs.bash ]}
          '';
        });
        familiar-render = (build "golem-familiar-render" [ "./cmd/golem-familiar-render" ]).overrideAttrs (old: {
          postInstall = (old.postInstall or "") + ''
            wrapProgram $out/bin/golem-familiar-render --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.tmux ]}
          '';
        });
      in {
        packages = { inherit cli service supervisor familiar-render; default = cli; };
        apps = {
          default = { type = "app"; program = "${cli}/bin/golem"; meta.description = "Control Golem delegated agents"; };
          golem-service = { type = "app"; program = "${service}/bin/golem-service"; };
          golem-supervisor = { type = "app"; program = "${supervisor}/bin/golem-supervisor"; };
          golem-familiar-render = { type = "app"; program = "${familiar-render}/bin/golem-familiar-render"; };
        };
        checks = {
          inherit cli service supervisor familiar-render;
          agent-hooks = pkgs.runCommand "golem-agent-hooks-tests" { nativeBuildInputs = [ pkgs.bun ]; } ''
            cp -r ${./integrations/pi/agent-hooks} ./agent-hooks
            chmod -R u+w ./agent-hooks
            bun test ./agent-hooks/events.test.ts
            touch $out
          '';
          familiar-extension = pkgs.runCommand "golem-familiar-extension-tests" { nativeBuildInputs = [ pkgs.bun ]; } ''
            cp -r ${./contrib/familiar} ./familiar
            chmod -R u+w ./familiar
            bun test ./familiar/pi/agents/resolve.test.ts ./familiar/pi/agents/cli.test.ts ./familiar/pi/agents/extension.test.ts ./familiar/manifest.test.ts
            touch $out
          '';
        };
        devShells.default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.tmux pkgs.git pkgs.bun pkgs.bashInteractive ];
          GOLEM_INTERACTIVE_SHELL = "${pkgs.bashInteractive}/bin/bash";
        };
      });
}
