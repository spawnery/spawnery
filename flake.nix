{
  description = "Spawnery — Kubernetes-natives Cloud-System für Minecraft-Netzwerke";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in
    {
      devShells = forAllSystems (pkgs:
        let
          # Linux: the nixpkgs packages, as before. envtest wants exactly
          # these three binaries in one directory.
          envtestFromNixpkgs = pkgs.runCommand "envtest-assets" { } ''
            mkdir -p $out
            ln -s ${pkgs.kubernetes}/bin/kube-apiserver $out/kube-apiserver
            ln -s ${pkgs.etcd}/bin/etcd                 $out/etcd
            ln -s ${pkgs.kubectl}/bin/kubectl           $out/kubectl
          '';

          # Darwin: nixpkgs does not build kube-apiserver there. The
          # controller-tools project publishes prebuilt binaries for
          # darwin/arm64; the hash is checked in, and the download only
          # happens when the derivation is built. The reverse does not
          # hold for Linux: those prebuilt binaries are dynamically linked
          # against glibc and would need autoPatchelfHook.
          envtestVersion = "1.36.2";
          envtestFromUpstream = pkgs.stdenvNoCC.mkDerivation {
            pname = "envtest-assets";
            version = envtestVersion;
            src = pkgs.fetchurl {
              url = "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v${envtestVersion}/envtest-v${envtestVersion}-darwin-arm64.tar.gz";
              hash = "sha256-80TnxwlhsQBHHu6k0lVQBvKCpqJ77Of0L77ed7KbiG4=";
            };
            sourceRoot = "controller-tools/envtest";
            dontConfigure = true;
            dontBuild = true;
            installPhase = ''
              mkdir -p $out
              install -m755 etcd kube-apiserver kubectl $out/
            '';
          };

          envtestAssets =
            if pkgs.stdenv.hostPlatform.isDarwin
            then envtestFromUpstream
            else envtestFromNixpkgs;
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gopls
              gotools
              golangci-lint
              gotestsum
              kubernetes-controller-tools
              kustomize
              kubectl
              kubernetes-helm
              kind
              k3d
            ];

            env = {
              KUBEBUILDER_ASSETS = "${envtestAssets}";
            };
          };
        });
    };
}
