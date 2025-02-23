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

        # Create a package for the specified system/platform
        makePlundrio = crossPkgs: crossPkgs.buildGoModule rec {
          pname = "plundrio";
          version = "0.9.3";
          src = ./.;
          vendorHash = "sha256-z7GGZ0j9DGuB14IYRGU2KqihL639d+7FplPtUGTRHsY=";
          proxyVendor = true;
          subPackages = [ "cmd/plundrio" ];

          env.CGO_ENABLED = "0";

          # Modified ldflags to work with pure Go builds
          ldflags = [
            "-X main.version=${version}"  # Inject version from package definition
          ];

          meta = with pkgs.lib; {
            description = "A Put.io integration for *arr applications";
            homepage = "https://github.com/elsbrock/plundrio";
            license = licenses.mit;
            maintainers = with maintainers; [ ];
            platforms = platforms.linux;
          };
        };

        # Create a docker image for the specified package
        makeDocker = pkg: architecture: pkgs.dockerTools.buildImage {
          name = "plundrio";
          tag = "latest";
          inherit architecture;
          copyToRoot = pkgs.buildEnv {
            name = "image-root";
            paths = [ pkg pkgs.cacert ];
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
    );
}
