{
  description = "e2b runtime — Nix dev environment for storage-layer work (fork: snajpa/runtime)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f:
        nixpkgs.lib.genAttrs systems (system: f (import nixpkgs { inherit system; }));

      devPackages = pkgs:
        let
          has = name: builtins.hasAttr name pkgs;
          maybe = names: map (name: builtins.getAttr name pkgs) (builtins.filter has names);
          # go.work requires Go >= 1.26.8 (see .tool-versions); nixpkgs may name
          # it go_1_26 or expose it as the default `go`.
          go = if has "go_1_26" then pkgs.go_1_26 else pkgs.go;
        in
        [
          go
          pkgs.git
          pkgs.gnumake
          pkgs.gcc
          pkgs.pkg-config
          pkgs.curl
          pkgs.unzip
        ] ++ maybe [
          "gopls"
          "gotools"
          "golangci-lint"
          "buf"
          "protobuf"
          "protoc-gen-go"
          "protoc-gen-go-grpc"
          "protoc-gen-connect-go"
          "nodejs_22"
          "bun"
          "python3"
          "uv"
          "google-cloud-sdk"
          "docker-client"
          "docker-compose"
          "qemu"
          "just"
          "fio"
          "jq"
          "yq-go"
          "shellcheck"
          "shfmt"
          "tmux"
          "liburing"
          "meson"
          "ninja"
          "sshpass"
        ];

      # The dev VM is Ubuntu 24.04 (e2b's production host floor), defined and
      # run through Nix: pinned cloud image + generated cloud-init + QEMU
      # runner. See nix/README.md and nix/ubuntu-vm.nix.
      vmPkgs = import nixpkgs { system = "x86_64-linux"; };
      ubuntuVm = import ./nix/ubuntu-vm.nix { pkgs = vmPkgs; };
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = devPackages pkgs;
          shellHook = ''
            echo "e2b dev shell — $(go version 2>/dev/null || echo 'go: missing')"
            echo "dev VM: nix build .#dev-vm && ./result/bin/e2b-dev-vm up"
          '';
        };
      });

      packages.x86_64-linux = {
        dev-vm = ubuntuVm.runner;
        dev-vm-image = ubuntuVm.image;
        dev-vm-seed = ubuntuVm.seed;
      };

      apps.x86_64-linux.dev-vm = {
        type = "app";
        program = "${ubuntuVm.runner}/bin/e2b-dev-vm";
      };
    };
}
