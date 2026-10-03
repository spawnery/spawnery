# What the game images share. The operator image takes only the identity
# (uid, gid, passwd, group): layeredImage below creates /data and /tmp and
# sets WorkingDir=/data, which is a game server's frame.
{ runCommand
, runtimeShell
, writeTextDir
, dockerTools
}:

rec {
  # runAsNonRoot needs a numeric user. Java runs without the passwd entry, but
  # a library's failing getpwuid would surface as an unrelated error.
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

  entrypointFrom = source: runCommand "spawnery-entrypoint" { } ''
    mkdir -p $out/usr/local/bin
    substitute ${source} $out/usr/local/bin/spawnery-entrypoint \
      --replace-fail '#!/bin/sh' '#!${runtimeShell}'
    chmod +x $out/usr/local/bin/spawnery-entrypoint
  '';

  # Copied, not symlinked, so the path is exactly the one internal/podspec names.
  binIn = { package, name }: runCommand "${name}-image-path" { } ''
    mkdir -p $out/usr/local/bin
    cp ${package}/bin/${name} $out/usr/local/bin/${name}
  '';

  # amd64 is only a label: buildLayeredImage does not cross-compile, and
  # flake.nix exposes the images on x86_64-linux only.
  layeredImage = { name, tag, contents, config }: dockerTools.buildLayeredImage {
    inherit name tag contents;
    architecture = "amd64";

    # Kubernetes mounts over /data and /tmp; these modes are for a plain
    # container runtime with a fresh volume, as in the image tests.
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
