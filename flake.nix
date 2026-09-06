{
  description = "Standalone Golem delegated-agent system";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };
  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        # claude-code is unfree; fort.tracked builds this flake purely, so the
        # allowance has to live here rather than in an env var.
        pkgs = import nixpkgs {
          inherit system;
          config.allowUnfreePredicate = pkg: builtins.elem (nixpkgs.lib.getName pkg) [ "claude-code" ];
        };
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
        herdrPlatform = {
          "x86_64-linux" = { artifact = "linux-x86_64"; hash = "sha256-l2FQoU1JDJSyQ+ouGn6y37Z/EuNrGC25CTb2co5q7PQ="; };
          "aarch64-linux" = { artifact = "linux-aarch64"; hash = "sha256-9VYQZY4cLg0qrvcwtLKriF9/i6AChas3K/sU8uPVtA0="; };
          "x86_64-darwin" = { artifact = "macos-x86_64"; hash = "sha256-q1AmLIGQzXqpBW0knSVcCMMow+hxbenPop208TG44sE="; };
          "aarch64-darwin" = { artifact = "macos-aarch64"; hash = "sha256-pdT01QTYswnJH4EQUFWTAPq6MSWEJfU8UIUvyW9q5XQ="; };
        }.${system};
        # Herdr is not in nixpkgs. Pin the official release artifact so golemd
        # never depends on an ambient/user install or version.
        herdr = pkgs.stdenvNoCC.mkDerivation {
          pname = "herdr";
          version = "0.8.2";
          src = pkgs.fetchurl {
            url = "https://github.com/herdrdev/herdr/releases/download/v0.8.2/herdr-${herdrPlatform.artifact}";
            inherit (herdrPlatform) hash;
          };
          dontUnpack = true;
          installPhase = ''install -Dm755 $src $out/bin/herdr'';
          meta = with pkgs.lib; { license = licenses.mit; platforms = platforms.unix; mainProgram = "herdr"; };
        };
        # Interactive bash reads bashrc even in non-login mode. Herdr accepts
        # only an executable path, so package a tiny no-profile/no-rc shell.
        herdrShell = pkgs.writeShellScriptBin "golem-herdr-shell" ''
          exec ${pkgs.bashInteractive}/bin/bash --noprofile --norc "$@"
        '';
        daemon = (build "golemd" [ "./cmd/golemd" ]).overrideAttrs (old: {
          postInstall = (old.postInstall or "") + ''
            wrapProgram $out/bin/golemd \
              --prefix PATH : ${pkgs.lib.makeBinPath [ herdr pkgs.tmux pkgs.git pkgs.bash pkgs.nix pkgs.claude-code ]} \
              --set-default GOLEM_HERDR ${herdr}/bin/herdr \
              --set-default GOLEM_HERDR_SHELL ${herdrShell}/bin/golem-herdr-shell
          '';
        });
        # Combined output for deployment: one profile carrying every Golem
        # binary, so trackers (e.g. fort-nix tracked services) can build a
        # single attr without CLI/daemon version skew.
        full = pkgs.symlinkJoin {
          name = "golem-full";
          paths = [ cli daemon herdr ];
        };
      in {
        packages = { inherit cli daemon full herdr; default = cli; };
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
