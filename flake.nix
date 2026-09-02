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
              # hack/publish-api.sh assembles one archive out of what Gradle
              # laid out, because the Central Portal takes a bundle rather than
              # a Maven deploy. `jar` from the JDK could make the same file and
              # would need a flag to stop it inventing a manifest -- a line the
              # next reader has to decode, to save a package this small.
              zip
              # And gpg, because hack/publish-api.sh signs with it rather than
              # with Gradle's signing plugin. Measured, on a real key: that
              # plugin reads a key through a bundled Bouncy Castle and answers
              # "Could not read PGP secret key" for keys recent GnuPG versions
              # write by default -- a failure in the middle of a build log,
              # about a format the person who made the key never chose. The
              # tool that wrote the key is the one that can read it.
              gnupg
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

          # Purpur, the fork this project's backend image is moving to. It
          # takes Paper's Mojang jar rather than pinning a second copy: both
          # are the same Minecraft version, there is only one such object, and
          # paperclip verifies it against its own download-context before
          # patching -- so a pair that ever drifts fails the build instead of
          # patching against the wrong original. See nix/purpur.nix.
          purpur = pkgs.callPackage ./nix/purpur.nix {
            inherit (paper) mojangJar;
          };

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
          # **0.2.10 moves this and only this**, which is a third mode the
          # separation had not yet been put through: nothing under agent/
          # changed, but image/entrypoint.sh did, and that ships inside the
          # game images rather than in the operator. So the two game tags move
          # and operatorVersion stands -- the mirror of 0.2.6 and 0.2.8, where
          # it stood and the operator moved.
          #
          # The sequence so far, and none of it is a miscount: 0.2.5, 0.2.7,
          # 0.2.9, 0.2.10, 0.2.12, 0.2.13, 0.2.15. 0.2.6, 0.2.8, 0.2.11 and
          # 0.2.14 built no game image, so
          # ghcr.io/spawnery/paper:26.2-0.2.6, -0.2.8, -0.2.11 and -0.2.14
          # simply do not exist, and each gap records a release that carried no
          # jar. 0.2.14's gap is the widest of them: its whole content was the
          # chart's own publication route.
          #
          # **0.2.15 moves this for the reason 0.2.10 did and one more.**
          # image/entrypoint.sh changed again -- it now takes the server jar
          # from SPAWNERY_SERVER_JAR -- so both game images differ. And a third
          # game image joins them: ghcr.io/spawnery/purpur, which shares this
          # number because it ships the same agent jar and is tagged the same
          # way.
          #
          # 0.2.16 moves it for a reason worth naming, because it is not the
          # obvious one: nothing under agent/ or image/ changed. internal/render
          # did, and that package is compiled into spawnery-config, which ships
          # in all three game images. A change to what the renderer accepts is
          # a change to those images even when the agent is byte-identical.
          #
          # A Paper or Purpur build that moves without this number moving would
          # collide, which is what makes a bump obligatory when the images
          # really do change rather than tidy.
          #
          # 0.2.17 is both halves of that at once. nix/paper-jre.nix gains
          # java.net.http, so the Paper and Purpur images run a different
          # runtime than 0.2.16 did -- and agent/ changed too: a renewal is no
          # longer reported as a stream failure. Either alone would oblige this
          # number; the operator is untouched, so operatorVersion below is not.
          #
          # 0.2.18 moves it again, and this time the other number with it: the
          # agents gained a verb. A server can publish a short state and a few
          # attributes, every agent reads them back out of the network picture,
          # and `/cloud info` prints what a server says about itself -- so the
          # jar inside the game images is a different jar.
          #
          # 0.2.19 moves it for the same kind of reason: the agents gained a
          # second verb -- a server can close its own door to new players and
          # open it again -- and a group's attributes reach a plugin through
          # the same jar.
          #
          # 0.2.20 moves it because a value the plugin API hands out gained a
          # component: a server now says which run of it this is.
          #
          # **0.2.21 moves this and nothing else, and the images it names are
          # identical to 0.2.20's.** What the release publishes is an artefact
          # that has never been published before: cloud.spawnery:spawnery-api,
          # whose coordinate takes its version from this number. The same shape
          # as 0.2.14, whose whole content was the chart's publication path.
          #
          # The two game images are pushed again under the new tag because they
          # are built from this number; the operator is not, and hack/publish.sh
          # refuses its unchanged tag on its own. Republishing two identical
          # images is the cost of having one number for the agent artefacts,
          # and it is smaller than giving the API a version of its own that
          # nothing else would keep in step.
          #
          # 0.2.22 exists because 0.2.21 did not do the one thing it was for.
          # hack/publish-api.sh read DRY_RUN as a presence rather than as a
          # value, and the workflow passes 0 for "really publish" -- so the
          # tagged release rehearsed, reported success and uploaded nothing.
          # The images this names are identical to 0.2.20's and 0.2.21's for
          # the third time, which is the price of the mistake and not of the
          # design.
          imageVersion = "0.2.22";

          # The operator's own version, deliberately not imageVersion.
          # imageVersion above is the *agent* version -- it reaches the
          # plugin's paper-plugin.yml and is reported to the operator as
          # Hello.version -- and hanging the operator's tag off it would mean a
          # fix in the reconciler claiming a new agent version, and an agent
          # release renaming an unchanged operator image.
          #
          # 0.2.6 was the first release to exercise that separation in the
          # direction it was built for: a reconciler change alone, with this
          # number moving and imageVersion standing still. 0.2.8 was the same
          # case. 0.2.7 and 0.2.9 move both, because both changed the operator
          # and the agents together -- 0.2.9 brings plugins from a volume on
          # this side and colour on the other.
          #
          # 0.2.10 did not move this -- its whole change was
          # image/entrypoint.sh, which nix/operator-image.nix does not
          # reference -- so **this number now has a gap too**, exactly as
          # imageVersion does: spawnery-operator:0.2.10 does not exist, and the
          # gap is the honest record of a release that built no operator.
          #
          # 0.2.11 moved it alone: the change was inside internal/controller and
          # nothing under agent/ or image/ was different. 0.2.12 moved both --
          # the chat feed's format is a Network field the operator carries and
          # the agents read.
          #
          # **0.2.13 does not move this.** Its change is in the shipped jars
          # (command replies wear the network's format) plus a comment on a CRD
          # field. A comment does not reach the compiled binary, so the
          # operator image is byte-identical and hack/publish.sh correctly
          # refuses to overwrite a tag a cluster has already pulled.
          #
          # 0.2.14 did not move it either -- it published the chart and nothing
          # else. **0.2.15 does**, and it is the first release since 0.2.9 to
          # move all three numbers at once: two new CRD fields the reconcilers
          # actually read (spec.env, and a claim source on spec.mounts), an
          # entrypoint that takes its jar from a variable, and a third game
          # image.
          #
          # 0.2.16 moves all three again: paper-world-defaults.yml became a
          # configOverlay key (internal/render, so the game images) and a mount
          # under /data/config is now refused (internal/podspec, so the
          # operator).
          #
          # 0.2.17 stood still here, for the reason the note above gives.
          # **0.2.18 moves it**: the announcement an agent sends is answered in
          # internal/agentserver, held in internal/agent and published by
          # internal/netstate. That is a reconciler-side change of the same
          # kind as any other, and it is why this release moves all three
          # numbers rather than only the images'.
          #
          # 0.2.19 moves all three again, and this one reaches further into the
          # operator than 0.2.18 did: the phase machine learns a door that
          # deregisters without moving a phase, the scaler stops counting seats
          # on a server no proxy will route to, and two group kinds gain a spec
          # field. **Unlike 0.2.18 the CRDs really change** -- an optional map
          # on ServerGroup and ProxyGroup -- so every object that exists
          # validates unchanged and the chart carries a new schema.
          #
          # 0.2.20 moves all three once more, and it is the smallest of the
          # three moves: the reconciler records which pod is behind a server
          # and netstate carries it onward. A status field, so the CRDs change
          # again -- and a status field is one the operator fills in, so no
          # object anybody wrote needs anything.
          operatorVersion = "0.2.20";

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
          # Exposed for the same reason paper-repo is: nix/purpur-jre.nix's
          # module list is a jdeps measurement over exactly these jars, and a
          # Purpur bump has to be able to repeat it.
          purpur-repo = purpur.repo;
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

          # The backend image this project is moving to. It shares
          # image/entrypoint.sh, the agent, both helper binaries and paper-jre
          # with the image above -- the module list was re-measured over
          # Purpur's own classpath and came out identical, see
          # nix/purpur-image.nix -- so what actually differs is the jar.
          purpur-image = pkgs.callPackage ./nix/purpur-image.nix {
            inherit purpur spawnery-slp spawnery-config agents imageVersion oci-common paper-jre;
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
