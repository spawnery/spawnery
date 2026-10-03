{
  description = "Spawnery — a Kubernetes-native cloud system for Minecraft networks";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in
    {
      devShells = forAllSystems (pkgs:
        let
          envtestFromNixpkgs = pkgs.runCommand "envtest-assets" { } ''
            mkdir -p $out
            ln -s ${pkgs.kubernetes}/bin/kube-apiserver $out/kube-apiserver
            ln -s ${pkgs.etcd}/bin/etcd                 $out/etcd
            ln -s ${pkgs.kubectl}/bin/kubectl           $out/kubectl
          '';

          # Darwin: nixpkgs does not build kube-apiserver there, so upstream's
          # prebuilt binaries (Linux ones would need autoPatchelfHook). The Linux
          # side follows nixpkgs' Kubernetes; bump envtestVersion along with it.
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
              # hack/publish.sh copies image archives straight from the Nix store; a
              # local container store in between could publish a stale image.
              skopeo
              # For hack/publish-api.sh's Central Portal bundle; `jar` would add a manifest.
              zip
              # hack/publish-api.sh signs with gpg: Gradle's signing plugin cannot read
              # keys in the format recent GnuPG writes by default.
              gnupg
              # protoc and protoc-gen-grpc-java are pinned again in
              # agent/common/build.gradle.kts (protobuf-java tracks protoc's X.Y,
              # io.grpc:grpc-* the generator's version); only this half moves with
              # nixpkgs. hack/toolchain-pins-agree.sh fails when they drift.
              protobuf
              protoc-gen-go
              protoc-gen-go-grpc
              protoc-gen-grpc-java
              gradle
              # mkdocs --strict is the link checker, so this is a test dependency too.
              python3Packages.mkdocs
              python3Packages.mkdocs-material
              python3Packages.mkdocs-mermaid2-plugin
              # hack/crd-docs.sh needs these; that mkdocs's closure carries them is incidental.
              python3
              python3Packages.pyyaml
              jdk21_headless
              jq
              # test/e2e/tutorial_test.go runs it as a subprocess, as a tutorial reader would.
              self.packages.${pkgs.system}.spawnery-join
            ];

            env = {
              KUBEBUILDER_ASSETS = "${envtestAssets}";
            };
          };
        });

      packages = forAllSystems (pkgs:
        let
          paper = pkgs.callPackage ./nix/paper.nix { };

          velocity = pkgs.callPackage ./nix/velocity.nix { };

          # Takes Paper's Mojang jar; paperclip verifies it before patching, so a
          # pair that drifts fails the build.
          purpur = pkgs.callPackage ./nix/purpur.nix {
            inherit (paper) mojangJar;
          };

          # The previous Minecraft version, built beside the current one so a
          # network can move on its own schedule.
          paper-26-2 = pkgs.callPackage ./nix/paper-26.2.nix { };
          purpur-26-2 = pkgs.callPackage ./nix/purpur-26.2.nix {
            inherit (paper-26-2) mojangJar;
          };

          oci-common = pkgs.callPackage ./nix/oci-common.nix { };

          paper-jre = pkgs.callPackage ./nix/paper-jre.nix { };

          velocity-jre = pkgs.callPackage ./nix/velocity-jre.nix { };

          # The agent and game-image version: it reaches paper-plugin.yml (reported
          # to the operator as Hello.version), the game image tags and
          # cloud.spawnery:spawnery-api. It moves, taking the release's number,
          # whenever anything under agent/, image/, internal/render or the JRE
          # derivations changes; a Paper or Purpur bump without it would collide
          # with a published tag. Gaps are releases that built no game image.
          imageVersion = "0.18.0";

          # The operator's version, separate from imageVersion so a reconciler fix
          # does not claim a new agent and an agent release does not rename an
          # unchanged operator image. It moves, taking the release's number, when
          # the operator binary changes (a comment does not). Gaps are releases
          # that built no operator; hack/publish.sh refuses an existing tag.
          operatorVersion = "0.18.0";

          spawnery-slp = pkgs.buildGoModule {
            pname = "spawnery-slp";
            version = "0.1.0";
            src = ./.;
            vendorHash = "sha256-q42rGVK1Mq2SGy2ZBMW8lHxXtpIrvjoRpetetK/wCs8=";
            subPackages = [ "cmd/spawnery-slp" ];
            # Static, because the image carries no libc of its own for it.
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" ];
          };

          # Test-only and deliberately in no image; hack/agent-test.sh runs it on the host.
          spawnery-stubop = pkgs.buildGoModule {
            pname = "spawnery-stubop";
            version = "0.2.0";
            src = ./.;
            vendorHash = "sha256-q42rGVK1Mq2SGy2ZBMW8lHxXtpIrvjoRpetetK/wCs8=";
            subPackages = [ "cmd/spawnery-stubop" ];
            env.CGO_ENABLED = 0;
          };

          # Test-only and in no image: an image carrying a tool that logs in as an
          # arbitrary player would hand an attacker one.
          spawnery-join = pkgs.buildGoModule {
            pname = "spawnery-join";
            version = "0.2.0";
            src = ./.;
            vendorHash = "sha256-q42rGVK1Mq2SGy2ZBMW8lHxXtpIrvjoRpetetK/wCs8=";
            subPackages = [ "cmd/spawnery-join" ];
            env.CGO_ENABLED = 0;
          };

          spawnery-config = pkgs.buildGoModule {
            pname = "spawnery-config";
            version = "0.1.0";
            src = ./.;
            vendorHash = "sha256-q42rGVK1Mq2SGy2ZBMW8lHxXtpIrvjoRpetetK/wCs8=";
            subPackages = [ "cmd/spawnery-config" ];
            # Static, because neither image carries a libc of its own for it.
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" ];
          };

          agents = pkgs.callPackage ./nix/agents.nix {
            inherit paper velocity imageVersion;
          };

          spawnery-operator = pkgs.buildGoModule {
            pname = "spawnery-operator";
            version = operatorVersion;
            src = ./.;
            vendorHash = "sha256-q42rGVK1Mq2SGy2ZBMW8lHxXtpIrvjoRpetetK/wCs8=";
            subPackages = [ "cmd/spawnery-operator" ];
            # Static, because the image carries no libc of its own for it.
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" ];
          };

          mermaid-js = pkgs.callPackage ./nix/mermaid.nix { };

          docs-fonts = pkgs.callPackage ./nix/fonts.nix { };

          agent-api-javadoc = pkgs.callPackage ./nix/agent-api-javadoc.nix { };

          docs-site = pkgs.callPackage ./nix/docs-site.nix { inherit mermaid-js docs-fonts agent-api-javadoc; };
        in
        {
          # Exposed on every system (they are jars) so a version bump can repeat
          # the measurements by hand: jdeps for the JRE module lists, and the one
          # above paperGlobalDefault in internal/render/paper_test.go.
          paper-repo = paper.repo;
          purpur-repo = purpur.repo;
          paper-jar = paper.paperJar;
          velocity-jar = velocity.jar;

          inherit spawnery-slp spawnery-stubop spawnery-join spawnery-config agents spawnery-operator mermaid-js docs-fonts agent-api-javadoc docs-site;
        } // pkgs.lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "x86_64-linux") {
          # buildLayeredImage does not cross-compile but labels its output amd64,
          # so the images exist only where that label is true.
          paper-image = pkgs.callPackage ./nix/paper-image.nix {
            inherit paper spawnery-slp spawnery-config agents imageVersion oci-common paper-jre;
          };

          purpur-image = pkgs.callPackage ./nix/purpur-image.nix {
            inherit purpur spawnery-slp spawnery-config agents imageVersion oci-common paper-jre;
          };

          # The same images over the 26.2 pins and the same agent jar: its
          # api-version 26.2 loads on both.
          paper-image-26-2 = pkgs.callPackage ./nix/paper-image.nix {
            paper = paper-26-2;
            inherit spawnery-slp spawnery-config agents imageVersion oci-common paper-jre;
          };
          purpur-image-26-2 = pkgs.callPackage ./nix/purpur-image.nix {
            purpur = purpur-26-2;
            inherit spawnery-slp spawnery-config agents imageVersion oci-common paper-jre;
          };

          # No spawnery-slp: a proxy's readiness is the agent's ready port.
          velocity-image = pkgs.callPackage ./nix/velocity-image.nix {
            inherit velocity spawnery-config agents imageVersion oci-common velocity-jre;
          };

          operator-image = pkgs.callPackage ./nix/operator-image.nix {
            inherit spawnery-operator operatorVersion oci-common;
          };

          docs-image = pkgs.callPackage ./nix/docs-image.nix {
            inherit docs-site oci-common;
          };
        });
    };
}
