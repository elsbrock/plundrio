{
  description = "put.io download client for *arr applications";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
        };
      in
      {
        packages = rec {
          plundrio-docker = pkgs.dockerTools.buildImage {
            name = "plundrio";
            tag = "latest";

            copyToRoot = pkgs.buildEnv {
              name = "image-root";
              paths = [ plundrio pkgs.cacert ];
              pathsToLink = [ "/bin" "/etc/ssl" ];
            };

            config = {
              Entrypoint = [ "/bin/plundrio" ];
              ExposedPorts = {
                "9091/tcp" = {};
              };
              Env = [
                "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
              ];
            };
          };

          plundrio = pkgs.buildGoModule {
            pname = "plundrio";
            version = "0.9.3";
            src = ./.;

            # Use the correct vendorHash provided by Nix
            vendorHash = "sha256-z7GGZ0j9DGuB14IYRGU2KqihL639d+7FplPtUGTRHsY=";
            proxyVendor = true;

            # Specify the correct package path
            subPackages = [ "cmd/plundrio" ];

            meta = with pkgs.lib; {
              description = "A Put.io integration for *arr applications";
              homepage = "https://github.com/elsbrock/plundrio";
              license = licenses.mit;
              maintainers = with maintainers; [ ];
            };
          };
          default = plundrio;
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
      });
}
