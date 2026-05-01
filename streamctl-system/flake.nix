{
  description = "streamctl — scheduled RTMP streamer with NixOS deployment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      # The streamctl Go package, parameterized by pkgs so it works on any system.
      streamctlPackage = pkgs: pkgs.buildGoModule {
        pname = "streamctl";
        version = "0.1.0";
        src = ./streamctl;
        # First build will fail with a hash mismatch; copy the suggested hash
        # from the error into here. Once go.sum is committed, this becomes
        # a pinned dependency hash.
        vendorHash = null;
        subPackages = [ "cmd" ];
        meta = with pkgs.lib; {
          description = "Schedule pre-recorded RTMP streams via systemd";
          license = licenses.mit;
          mainProgram = "cmd";
        };
      };

      # The NixOS module, lifted to the top level so the host config can
      # import it directly without going through a separate flake.
      streamctlModule = { config, lib, pkgs, ... }:
        let
          cfg = config.services.streamctl;
          pkg = streamctlPackage pkgs;
        in
        {
          options.services.streamctl = {
            enable = lib.mkEnableOption "streamctl scheduled RTMP streamer";

            listen = lib.mkOption {
              type = lib.types.str;
              default = "127.0.0.1:8080";
              description = "Address:port for the web UI to listen on. Put a reverse proxy in front for TLS.";
            };

            secretFile = lib.mkOption {
              type = lib.types.path;
              description = ''
                Path to a file containing the login secret for the web UI.
                File should contain a single line with the secret. Mode 0400, owned by the streamctl user.
              '';
              example = "/var/lib/streamctl/secret";
            };

            videoDir = lib.mkOption {
              type = lib.types.str;
              default = "/var/lib/streamctl/videos";
              description = "Directory where video files are uploaded via scp.";
            };

            dataDir = lib.mkOption {
              type = lib.types.str;
              default = "/var/lib/streamctl";
              description = "Directory for the SQLite database.";
            };

            user = lib.mkOption {
              type = lib.types.str;
              default = "streamctl";
              description = "User the ffmpeg processes run as.";
            };

            group = lib.mkOption {
              type = lib.types.str;
              default = "streamctl";
              description = "Group for the streamctl user.";
            };

            openFirewall = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = "Open the listen port in the firewall. Usually you want a reverse proxy instead.";
            };
          };

          config = lib.mkIf cfg.enable {
            users.users.${cfg.user} = {
              isSystemUser = true;
              group = cfg.group;
              home = cfg.dataDir;
              createHome = false;
              description = "streamctl service user";
              useDefaultShell = true; # so scp uploads to videoDir work
            };

            users.groups.${cfg.group} = { };

            systemd.tmpfiles.rules = [
              "d ${cfg.dataDir} 0750 ${cfg.user} ${cfg.group} - -"
              "d ${cfg.videoDir} 0750 ${cfg.user} ${cfg.group} - -"
            ];

            systemd.services.streamctl = {
              description = "streamctl web UI";
              after = [ "network.target" ];
              wantedBy = [ "multi-user.target" ];

              path = [ pkgs.systemd pkgs.ffmpeg ];

              serviceConfig = {
                Type = "simple";
                # Root: needed to write systemd units and run systemctl.
                # ffmpeg processes drop to cfg.user via the generated units.
                User = "root";
                Group = "root";
                ExecStart = ''
                  ${pkg}/bin/cmd \
                    -listen ${cfg.listen} \
                    -db ${cfg.dataDir}/streamctl.db \
                    -video-dir ${cfg.videoDir} \
                    -unit-dir /etc/systemd/system \
                    -unit-prefix streamctl- \
                    -run-user ${cfg.user}
                '';
                EnvironmentFile = "-/run/streamctl/env";
                Restart = "on-failure";
                RestartSec = 5;
              };

              preStart = ''
                mkdir -p /run/streamctl
                umask 077
                echo "STREAMCTL_SECRET=$(cat ${cfg.secretFile})" > /run/streamctl/env
                chmod 0400 /run/streamctl/env
              '';
            };

            networking.firewall = lib.mkIf cfg.openFirewall {
              allowedTCPPorts = [
                (lib.toInt (lib.last (lib.splitString ":" cfg.listen)))
              ];
            };
          };
        };
    in
    # Per-system outputs (the package, devshell).
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages.default = streamctlPackage pkgs;
        packages.streamctl = streamctlPackage pkgs;

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            sqlite
            ffmpeg
            terraform
            doctl
            gnumake
            openssl
            nixos-rebuild
          ];
        };
      }
    ) // {
      # System-independent outputs (NixOS modules and configurations).
      nixosModules.default = streamctlModule;
      nixosModules.streamctl = streamctlModule;

      # The host configuration. `make deploy` builds this and pushes it.
      nixosConfigurations.streamctl = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          streamctlModule
          ./nixos/configuration.nix
        ];
      };
    };
}
