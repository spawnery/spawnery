# The operator image: a static binary, no shell, no writable directory. It
# takes only the identity from oci-common, not layeredImage, whose /data and
# /tmp hack/operator-image-test.sh refuses.
{ dockerTools
, spawnery-operator
, operatorVersion
, oci-common
}:

dockerTools.buildLayeredImage {
  name = "ghcr.io/spawnery/spawnery-operator";
  tag = operatorVersion;

  # A label, not a cross-compile; see nix/oci-common.nix.
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
