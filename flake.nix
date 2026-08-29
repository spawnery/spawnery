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
          #
          # The two halves drift on their own. A new Kubernetes version
          # arriving through the nixpkgs channel moves the Linux path and
          # leaves envtestVersion below exactly where it was, so the two
          # development environments run different kube-apiserver versions
          # against the same suite with nothing saying so. Bump both together.
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
              # hack/publish.sh copies each image archive straight from the Nix
              # store to the registry. A local container store in between would
              # publish whatever a stale `podman load` left behind rather than
              # what the flake describes.
              skopeo
              # Both of these are pinned a second time, by version, in
              # agent/common/build.gradle.kts -- and only this half moves when
              # nixpkgs does. `protobuf` here is protoc, whose X.Y the
              # `protobuf-java` artifact tracks one for one (protoc 35.1 <->
              # protobuf-java 4.35.1); `protoc-gen-grpc-java` here is the
              # generator whose output the `io.grpc:grpc-*` artifacts have to
              # match, currently 1.83.1. A `nix flake update` followed by
              # `make proto` can therefore regenerate stubs that demand a
              # runtime the build does not resolve, and the symptom
              # (`compileProtoJava`: cannot find symbol, or a
              # ProtobufRuntimeVersionException at class init) appears nowhere
              # near this line. After a flake update, read both new versions
              # from the repository root and move the literals in
              # agent/common/build.gradle.kts's dependencies block to match.
              # protoc answers for itself:
              #
              #   nix develop -c protoc --version
              #
              # The generator plugin takes no option at all, so it is read off
              # the pinned nixpkgs instead:
              #
              #   nix eval --raw --impure --expr '(builtins.getFlake (toString ./.)).inputs.nixpkgs.legacyPackages.${builtins.currentSystem}.protoc-gen-grpc-java.version'
              #
              # Nothing enforces this but flake.lock. See docs/known-issues.md.
              protobuf
              protoc-gen-go
              protoc-gen-go-grpc
              protoc-gen-grpc-java
              gradle
              jdk21_headless
              # hack/agent-test.sh asserts on the stub operator's event stream.
              jq
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

          # Extracted while paper-image was the only consumer; velocity-image
          # will be the second (see nix/oci-common.nix for why that timing
          # matters).
          oci-common = pkgs.callPackage ./nix/oci-common.nix { };

          # The Paper image's Java runtime, jlink'd to the modules Paper and
          # the agent actually resolve. Its own file because the list is
          # measured rather than chosen, and that measurement is what a Paper
          # bump has to repeat.
          paper-jre = pkgs.callPackage ./nix/paper-jre.nix { };

          # Velocity's counterpart. Separate because the classpaths are, and
          # each list is a measurement over its own.
          velocity-jre = pkgs.callPackage ./nix/velocity-jre.nix { };

          # The one place this version is written down. It reaches both the
          # plugin's paper-plugin.yml (which the agent reports to the
          # operator as Hello.version) and the image tag, so the two can
          # never drift apart the way the agent derivation's and
          # paper-image.nix's separate defaults once could.
          #
          # **0.2.7 moves it and 0.2.6 did not**, which is the separation
          # working in both directions within two releases. 0.2.6 was milestone
          # 7a, entirely inside internal/controller: nothing under agent/
          # changed, so both game images kept the 0.2.5 tags a cluster had
          # already pulled. Milestone 7c is the opposite -- a /cloud command
          # tree, a chat feed and an EventBus, all of it inside the shipped
          # jars -- so the images really do change and the tag has to say so.
          #
          # It skips 0.2.6 rather than counting to it. The tag is
          # <upstream>-<imageVersion>, so ghcr.io/spawnery/paper:26.2-0.2.6
          # simply does not exist, and that gap is the honest record: there was
          # no agent build in that release. Numbering it 0.2.6 now would name a
          # jar after a release that never contained one.
          #
          # A Paper build that moves without this number moving would collide
          # the same way, which is what makes a bump obligatory when the images
          # really do change rather than tidy.
          imageVersion = "0.2.7";

          # The operator's own version, deliberately not imageVersion.
          # imageVersion above is the *agent* version -- it reaches the
          # plugin's paper-plugin.yml and is reported to the operator as
          # Hello.version -- and hanging the operator's tag off it would mean a
          # fix in the reconciler claiming a new agent version, and an agent
          # release renaming an unchanged operator image.
          #
          # 0.2.6 was the first release to exercise that separation in the
          # direction it was built for: a reconciler change alone, with this
          # number moving and imageVersion standing still. 0.2.7 moves both,
          # because milestone 7c changed both sides -- the operator gained a
          # ScaleBoost reader, a writing request path and a CloudEvent
          # recorder, and the agents gained everything that reads them.
          operatorVersion = "0.2.7";

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

          # Test-only, and deliberately not referenced by nix/paper-image.nix:
          # the operator's counterpart has no business inside a server image.
          # hack/agent-test.sh runs it on the host.
          spawnery-stubop = pkgs.buildGoModule {
            pname = "spawnery-stubop";
            version = "0.2.0";
            src = ./.;
            vendorHash = "sha256-q42rGVK1Mq2SGy2ZBMW8lHxXtpIrvjoRpetetK/wCs8=";
            subPackages = [ "cmd/spawnery-stubop" ];
            env.CGO_ENABLED = 0;
          };

          # Test-only for the same reason as spawnery-stubop, and likewise in
          # no image: it is the automated half of milestone 3's success
          # criterion, run from a developer machine or the evidence runbook
          # against a proxy's NodePort. An image that carried a tool for
          # logging in as an arbitrary player would be handing an attacker
          # one.
          spawnery-join = pkgs.buildGoModule {
            pname = "spawnery-join";
            version = "0.2.0";
            src = ./.;
            vendorHash = "sha256-q42rGVK1Mq2SGy2ZBMW8lHxXtpIrvjoRpetetK/wCs8=";
            subPackages = [ "cmd/spawnery-join" ];
            env.CGO_ENABLED = 0;
          };

          # Baked into both the Paper and Velocity images; it writes the
          # configuration each JVM actually reads, before the JVM starts. See
          # internal/render and cmd/spawnery-config.
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

          # `paper` and `velocity` stay explicit arguments even though neither
          # is part of the version string: pkgs.callPackage fills arguments
          # from pkgs only, and both are local let bindings. What they are for
          # is the postPatch symlinks that hand Gradle each platform's API.
          agents = pkgs.callPackage ./nix/agents.nix {
            inherit paper velocity imageVersion;
          };

          # The operator itself, packaged so an image can be built from it.
          # `make build` still exists for the local loop; this is the same
          # binary produced reproducibly, which is what nix/operator-image.nix
          # and hack/publish.sh need.
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
        in
        {
          # Architecture-independent (it is jars), so this stays available on
          # every system.
          paper-repo = paper.repo;
          # The paperclip launcher, exposed for the same reason velocity-jar
          # is: it is what a human runs by hand to measure something out of
          # the pinned build. The command that uses it is recorded above
          # paperGlobalDefault in internal/render/paper_test.go, which
          # regenerates internal/render/testdata/paper-global.default.yml.
          paper-jar = paper.paperJar;
          velocity-jar = velocity.jar;

          inherit spawnery-slp spawnery-stubop spawnery-join spawnery-config agents spawnery-operator;
        } // pkgs.lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "x86_64-linux") {
          # dockerTools.buildLayeredImage packs the host's binaries under a
          # fixed "amd64" label (see nix/paper-image.nix); it does not
          # cross-compile. Restricting the attribute to x86_64-linux here,
          # rather than building it everywhere and hoping the label is
          # accurate, is what keeps that label true: on every other system the
          # attribute is simply absent, so `nix build .#paper-image` fails
          # with "does not provide attribute" instead of quietly producing a
          # mislabelled image. `nix flake show` and `nix develop` stay
          # unaffected elsewhere.
          paper-image = pkgs.callPackage ./nix/paper-image.nix {
            inherit paper spawnery-slp spawnery-config agents imageVersion oci-common paper-jre;
          };

          # No spawnery-slp: a proxy's readiness is the agent's ready port,
          # not a server list ping, so the image needs no pinger. The agent
          # itself now ships -- it is the thing that binds that port.
          velocity-image = pkgs.callPackage ./nix/velocity-image.nix {
            inherit velocity spawnery-config agents imageVersion oci-common velocity-jre;
          };

          # Restricted to x86_64-linux for the same reason the two game images
          # are, and the reason is in their comment above: buildLayeredImage
          # does not cross-compile but labels its output amd64 regardless.
          operator-image = pkgs.callPackage ./nix/operator-image.nix {
            inherit spawnery-operator operatorVersion oci-common;
          };
        });
    };
}
