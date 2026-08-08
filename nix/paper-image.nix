# The Paper base image.
#
# Everything the podspec prescribes has to be satisfied here, because the pod
# spec is already written: /usr/local/bin/spawnery-slp, port 25565, working
# directory /data, scratch /tmp, a numeric user, and nothing else writable.
{ bash
, buildEnv
, coreutils
, dockerTools
, gnugrep
, jdk25_headless
, runCommand
, runtimeShell
, writeTextDir
, paper
, spawnery-slp
, imageVersion ? "0.1.0"
}:

let
  # runAsNonRoot refuses to start an image with no numeric user. The probe
  # measured that Java itself does not need the passwd entry as long as HOME is
  # set; it is here because a failing getpwuid inside a library on the
  # classpath surfaces as an error that says nothing about its cause.
  passwd = writeTextDir "etc/passwd" ''
    root:x:0:0:root:/root:/bin/sh
    spawnery:x:10001:10001:spawnery:/data:/bin/sh
  '';

  group = writeTextDir "etc/group" ''
    root:x:0:
    spawnery:x:10001:
  '';

  # The shebang is rewritten to a shell that exists in this image. Relying on
  # /bin/sh in a Nix-built image would work today and break the day the tool
  # set changes.
  entrypoint = runCommand "spawnery-entrypoint" { } ''
    mkdir -p $out/usr/local/bin
    substitute ${../image/entrypoint.sh} $out/usr/local/bin/spawnery-entrypoint \
      --replace-fail '#!/bin/sh' '#!${runtimeShell}'
    chmod +x $out/usr/local/bin/spawnery-entrypoint
  '';

  # Copied rather than symlinked, so the path in the image is exactly the one
  # internal/podspec names and does not depend on a store link resolving.
  slp = runCommand "spawnery-slp-image-path" { } ''
    mkdir -p $out/usr/local/bin
    cp ${spawnery-slp}/bin/spawnery-slp $out/usr/local/bin/spawnery-slp
  '';

  paperHome = runCommand "paper-home" { } ''
    mkdir -p $out/opt/paper
    cp ${paper.paperJar} $out/opt/paper/paper.jar
    cp -r ${paper.repo} $out/opt/paper/repo
    chmod -R a-w $out/opt/paper
  '';
in
dockerTools.buildLayeredImage {
  name = "ghcr.io/spawnery/paper";
  tag = "${paper.paperVersion}-${imageVersion}";

  # amd64 explicitly, not the host's architecture: this is only a label and
  # dockerTools.buildLayeredImage does not cross-compile, so it is only true
  # because flake.nix exposes `packages.paper-image` exclusively on
  # x86_64-linux (see the comment there). If this derivation is ever called
  # from elsewhere, that guarantee has to move with it.
  architecture = "amd64";

  # Ordered by rate of change. The JRE and the patched Paper repo are large and
  # almost static; our own two files are small and change per commit. Milestone
  # 2c adds the agent plugin as another small layer without touching either.
  #
  # dockerTools.buildLayeredImage takes this list under `contents`, not
  # `copyToRoot` — `copyToRoot` belongs to buildImage, one function up in the
  # same file. Passing it here throws "called with unexpected argument
  # 'copyToRoot'" before a single layer is built; verified against the
  # nixpkgs revision this flake pins (b7c2ada, see flake.lock).
  contents = [
    (buildEnv {
      name = "paper-tools";
      # grep and mv come from coreutils and gnugrep because the entrypoint uses
      # them; bash because the entrypoint's shebang points at it.
      paths = [ bash coreutils gnugrep jdk25_headless ];
      pathsToLink = [ "/bin" ];
    })
    passwd
    group
    paperHome
    slp
    entrypoint
  ];

  # /data and /tmp are always mounted over in Kubernetes, so their mode there
  # comes from the kubelet, which creates an emptyDir world-writable. The mode
  # set here is what makes the same image usable under a plain container
  # runtime with a fresh volume — which is exactly what make image-test does.
  extraCommands = ''
    mkdir -p data tmp
    chmod 0777 data
    chmod 1777 tmp
  '';

  config = {
    User = "10001:10001";
    WorkingDir = "/data";
    Env = [
      "HOME=/data"
      "PATH=/bin:/usr/local/bin"
      "PAPER_HOME=/opt/paper"
    ];
    ExposedPorts = { "25565/tcp" = { }; };
    Entrypoint = [ "/usr/local/bin/spawnery-entrypoint" ];
    Labels = {
      "org.opencontainers.image.title" = "Spawnery Paper base image";
      "org.opencontainers.image.version" = "${paper.paperVersion}-${imageVersion}";
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
      # The Paper build number lives here rather than in the tag, so an
      # upstream rebuild does not force every sample manifest to be touched.
      "cloud.spawnery.paper-build" = paper.paperBuild;
    };
  };
}
