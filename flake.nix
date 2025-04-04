{
  description = "put.io download client for *arr applications";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };
  outputs = { self, nixpkgs, flake-utils }:
    let
      pname = "plundrio";
      version = "0.10.4";
      description = "A Put.io integration for *arr applications";
      maintainer = {
        name = "Simon Elsbrock";
        email = "simon@iodev.org";
      };
      license = "mit";

      nixosModule = { config, lib, pkgs, ... }:
        let
          cfg = config.services.plundrio;
        in
        {
          options.services.plundrio = {
            enable = lib.mkEnableOption "plundrio put.io download client";

            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.system}.plundrio;
              defaultText = "self.packages.${pkgs.system}.plundrio";
              description = "The plundrio package to use.";
            };

            targetDir = lib.mkOption {
              type = lib.types.path;
              description = "Target directory for downloads";
              example = "/var/lib/plundrio/downloads";
            };

            putioFolder = lib.mkOption {
              type = lib.types.str;
              default = "plundrio";
              description = "Put.io folder name";
            };

            authTokenFile = lib.mkOption {
              type = lib.types.path;
              description = "Path to file containing Put.io OAuth token";
              example = "/run/credentials/plundrio.service/token";
            };

            listenAddr = lib.mkOption {
              type = lib.types.str;
              default = ":9091";
              description = "Listen address for the Transmission RPC server";
              example = "127.0.0.1:9091";
            };

            workerCount = lib.mkOption {
              type = lib.types.int;
              default = 4;
              description = "Number of download workers";
            };

            logLevel = lib.mkOption {
              type = lib.types.enum [ "trace" "debug" "info" "warn" "error" "fatal" "panic" "none" "pretty" ];
              default = "info";
              description = "Log level";
            };

            user = lib.mkOption {
              type = lib.types.str;
              default = "plundrio";
              description = "User account under which plundrio runs";
            };

            group = lib.mkOption {
              type = lib.types.str;
              default = "plundrio";
              description = "Group under which plundrio runs";
            };
          };

          config = lib.mkIf cfg.enable {
            # Create user and group if they don't exist
            users.users = lib.mkIf (cfg.user == "plundrio") {
              plundrio = {
                isSystemUser = true;
                group = cfg.group;
                description = "plundrio service user";
                home = "/var/lib/plundrio";
                createHome = true;
              };
            };

            users.groups = lib.mkIf (cfg.group == "plundrio") {
              plundrio = {};
            };

            # Create the target directory if it doesn't exist
            systemd.tmpfiles.rules = [
              "d '${cfg.targetDir}' 0750 ${cfg.user} ${cfg.group} - -"
            ];

            # Define the systemd service
            systemd.services.plundrio = {
              description = "plundrio put.io download client";
              wantedBy = [ "multi-user.target" ];
              after = [ "network.target" ];

              serviceConfig = {
                Type = "simple";
                User = cfg.user;
                Group = cfg.group;
                LoadCredential = [ "token:${cfg.authTokenFile}" ];
                ExecStart = ''
                  ${cfg.package}/bin/plundrio run \
                    --target ${lib.escapeShellArg cfg.targetDir} \
                    --folder ${lib.escapeShellArg cfg.putioFolder} \
                    --listen ${lib.escapeShellArg cfg.listenAddr} \
                    --workers ${toString cfg.workerCount} \
                    --log-level ${cfg.logLevel}
                '';
                Environment = [
                  "PLDR_TOKEN_FILE=%d/token"
                ];
                Restart = "on-failure";
                RestartSec = "10s";

                # Security hardening
                CapabilityBoundingSet = "";
                DeviceAllow = "";
                LockPersonality = true;
                MemoryDenyWriteExecute = true;
                NoNewPrivileges = true;
                PrivateDevices = true;
                PrivateTmp = true;
                ProtectClock = true;
                ProtectControlGroups = true;
                ProtectHome = true;
                ProtectHostname = true;
                ProtectKernelLogs = true;
                ProtectKernelModules = true;
                ProtectKernelTunables = true;
                ProtectSystem = "strict";
                ReadWritePaths = [ cfg.targetDir ];
                RemoveIPC = true;
                RestrictAddressFamilies = [ "AF_INET" "AF_INET6" ];
                RestrictNamespaces = true;
                RestrictRealtime = true;
                RestrictSUIDSGID = true;
                SystemCallArchitectures = "native";
                SystemCallFilter = [ "@system-service" "~@privileged" ];
                UMask = "0027";
              };
            };
          };
        };
    in
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
        };

        # Create a package for the specified system/platform
        makePlundrio = crossPkgs: crossPkgs.buildGoModule rec {
          inherit pname version;
          src = ./.;
          vendorHash = "sha256-tUvjxuUk79iQokx9SoifLI/8t8Au3r3ipgqAJ2JwBS8=";
          proxyVendor = true;
          subPackages = [ "cmd/${pname}" ];

          # Modified ldflags to work with pure Go builds
          ldflags = [
            "-X main.version=${version}"
          ];

          meta = with pkgs.lib; {
            inherit description;
            homepage = "https://github.com/elsbrock/${pname}";
            license = licenses.${license};
            maintainers = [ maintainer ];
            platforms = platforms.linux;
          };
        };

        # Create a docker image for the specified package
        makeDocker = pkg: architecture: pkgs.dockerTools.buildImage {
          name = pname;
          tag = "latest";
          inherit architecture;
          copyToRoot = pkgs.buildEnv {
            name = "image-root";
            paths = [ pkg pkgs.cacert ];
            pathsToLink = [ "/bin" "/etc/ssl" ];
          };
          config = {
            Entrypoint = [ "/bin/plundrio" ];
            Cmd = [ "run" ];
            ExposedPorts = {
              "9091/tcp" = {};
            };
            Env = [
              "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
            ];
          };
        };
      in
      {
        packages = {
          # Native build
          plundrio = makePlundrio pkgs;
          plundrio-docker = makeDocker self.packages.${system}.plundrio "amd64";

          # Cross-compilation for aarch64
          plundrio-aarch64 = makePlundrio (pkgs.pkgsCross.aarch64-multiplatform);
          plundrio-docker-aarch64 = makeDocker self.packages.${system}.plundrio-aarch64 "arm64";

          default = self.packages.${system}.plundrio;
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_1_21
            gopls
            go-tools
            golangci-lint
          ];
          shellHook = ''
            echo "plundrio development shell"
            echo "Go $(go version)"
          '';
        };
      }
    ) // {
      # Export the NixOS module
      nixosModules.default = nixosModule;
      nixosModule = nixosModule; # For backwards compatibility
    };
}
