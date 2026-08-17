# What both Spawnery *game* images need verbatim.
#
# Extracted while there was exactly one consumer, which is the only cheap time
# to do it: two images that each grew their own copy of the numeric user or the
# binary path would still start, and would differ in ways that only surface as
# a pod that runs as the wrong uid or an exec probe that cannot find its tool.
#
# The operator image is a third consumer and takes only the identity from here
# -- uid, gid, passwd, group -- because layeredImage below is a game server's
# frame: it creates /data and /tmp and sets WorkingDir=/data. See
# nix/operator-image.nix, which builds its own. That absence is deliberate and
# not an oversight.
{ runCommand
, runtimeShell
, writeTextDir
, dockerTools
}:

rec {
  # runAsNonRoot refuses to start an image with no numeric user. The probe in
  # milestone 2b measured that Java itself does not need the passwd entry as
  # long as HOME is set; it is here because a failing getpwuid inside a library
  # on the classpath surfaces as an error that says nothing about its cause.
  uid = 10001;
  gid = 10001;

  passwd = writeTextDir "etc/passwd" ''
    root:x:0:0:root:/root:/bin/sh
    spawnery:x:${toString uid}:${toString gid}:spawnery:/data:/bin/sh
  '';

  group = writeTextDir "etc/group" ''
    root:x:0:
    spawnery:x:${toString gid}:
  '';

  # The shebang is rewritten to a shell that exists in this image. Relying on
  # /bin/sh in a Nix-built image would work today and break the day the tool
  # set changes.
  entrypointFrom = source: runCommand "spawnery-entrypoint" { } ''
    mkdir -p $out/usr/local/bin
    substitute ${source} $out/usr/local/bin/spawnery-entrypoint \
      --replace-fail '#!/bin/sh' '#!${runtimeShell}'
    chmod +x $out/usr/local/bin/spawnery-entrypoint
  '';

  # Copied rather than symlinked, so the path in the image is exactly the one
  # internal/podspec names and does not depend on a store link resolving.
  binIn = { package, name }: runCommand "${name}-image-path" { } ''
    mkdir -p $out/usr/local/bin
    cp ${package}/bin/${name} $out/usr/local/bin/${name}
  '';

  # The frame both images share. Callers pass what differs: the name, the tag,
  # the contents and the environment.
  #
  # architecture is amd64 explicitly rather than the host's: this is only a
  # label, dockerTools.buildLayeredImage does not cross-compile, and it is only
  # true because flake.nix exposes the image attributes exclusively on
  # x86_64-linux. If an image derivation is ever called from elsewhere, that
  # guarantee has to move with it.
  layeredImage = { name, tag, contents, config }: dockerTools.buildLayeredImage {
    inherit name tag contents;
    architecture = "amd64";

    # /data and /tmp are always mounted over in Kubernetes, so their mode there
    # comes from the kubelet, which creates an emptyDir world-writable. The mode
    # set here is what makes the same image usable under a plain container
    # runtime with a fresh volume — which is exactly what the image tests do.
    extraCommands = ''
      mkdir -p data tmp
      chmod 0777 data
      chmod 1777 tmp
    '';

    config = {
      User = "${toString uid}:${toString gid}";
      WorkingDir = "/data";
    } // config;
  };
}
