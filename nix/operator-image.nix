# The operator image.
#
# Unlike the two game images this one carries no runtime, no shell and no
# writable directory: the operator is a single static binary that talks to the
# API server and writes nothing to disk. It therefore takes from oci-common
# only the identity -- the numeric user and its passwd/group entries, so all
# three images run as the same uid and runAsNonRoot has an entry to resolve --
# and builds its own frame.
#
# oci-common.layeredImage is deliberately not used: it creates /data and /tmp,
# chmods them, and sets WorkingDir=/data. That is a game server's shape. An
# operator running with readOnlyRootFilesystem and no state would carry two
# directories nothing reads, and hack/operator-image-test.sh fails if one
# appears.
{ dockerTools
, spawnery-operator
, operatorVersion
, oci-common
}:

dockerTools.buildLayeredImage {
  name = "ghcr.io/spawnery/spawnery-operator";
  tag = operatorVersion;

  # A label, not a cross-compile, exactly as in nix/oci-common.nix: it is only
  # true because flake.nix exposes this attribute on x86_64-linux alone.
  architecture = "amd64";

  contents = [
    oci-common.passwd
    oci-common.group
    (oci-common.binIn { package = spawnery-operator; name = "spawnery-operator"; })
  ];

  config = {
    User = "${toString oci-common.uid}:${toString oci-common.gid}";
    WorkingDir = "/";
    Entrypoint = [ "/usr/local/bin/spawnery-operator" ];
    # Declared for documentation; the Deployment names all three itself.
    ExposedPorts = {
      "8080/tcp" = { };
      "8081/tcp" = { };
      "9443/tcp" = { };
    };
    Labels = {
      "org.opencontainers.image.title" = "Spawnery operator";
      "org.opencontainers.image.version" = operatorVersion;
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
    };
  };
}
