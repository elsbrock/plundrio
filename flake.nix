{
  description = "plundrio - A Put.io integration for *arr applications";

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
              paths = [ plundrio ];
              pathsToLink = [ "/bin" ];
            };

            config = {
              Entrypoint = [ "/bin/plundrio" ];
              ExposedPorts = {
                "9091/tcp" = {};
              };
            };
          };

          plundrio = pkgs.buildGoModule {
            pname = "plundrio";
            version = "0.1.0";
            src = ./.;

            # Use the correct vendorHash provided by Nix
            vendorHash = "sha256-0poj90E4qfG+XU1yzHddT/sKwu3jis1iD0OE22l9XYo=";
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
