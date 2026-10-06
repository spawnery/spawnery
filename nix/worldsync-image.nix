# The node agent for ObjectStore worlds: a static binary, run as root in a
# privileged container because it bind-mounts into the kubelet's pod
# directories. No /tmp: the import Job mounts an emptyDir there.
{ dockerTools
, spawnery-worldsync
, operatorVersion
, oci-common
}:

dockerTools.buildLayeredImage {
  name = "ghcr.io/spawnery/spawnery-worldsync";
  tag = operatorVersion;
  architecture = "amd64";
  contents = [
    oci-common.passwd
    oci-common.group
    (oci-common.binIn { package = spawnery-worldsync; name = "spawnery-worldsync"; })
  ];
  config = {
    User = "0:0";
    WorkingDir = "/";
    Entrypoint = [ "/usr/local/bin/spawnery-worldsync" ];
    Labels = {
      "org.opencontainers.image.title" = "Spawnery world sync";
      "org.opencontainers.image.version" = operatorVersion;
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
    };
  };
}
