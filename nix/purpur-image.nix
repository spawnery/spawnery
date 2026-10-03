# The Purpur base image: nix/paper-image.nix with the jar swapped. A separate
# file rather than a shared function because paper-image.nix is deprecated
# and will be deleted.
{ bash
, buildEnv
, coreutils
, findutils
, paper-jre
, runCommand
, purpur
, spawnery-slp
, spawnery-config
, agents
, imageVersion
, oci-common
}:

let
  purpurHome = runCommand "purpur-home" { } ''
    mkdir -p $out/opt/purpur
    cp ${purpur.purpurJar} $out/opt/purpur/purpur.jar
    cp -r ${purpur.repo} $out/opt/purpur/repo
    chmod -R a-w $out/opt/purpur
  '';

  agent = runCommand "purpur-agent-image-path" { } ''
    mkdir -p $out/opt/purpur/agent
    cp ${agents}/share/spawnery/paper/spawnery-agent.jar $out/opt/purpur/agent/spawnery-agent.jar
  '';
in
oci-common.layeredImage {
  name = "ghcr.io/spawnery/purpur";
  tag = "${purpur.purpurVersion}-${imageVersion}";

  contents = [
    (buildEnv {
      name = "purpur-tools";
      # paper-jre: jdeps over Purpur's own classpath gave Paper's module list
      # exactly; repeat that on a Purpur bump. findutils as in paper-image.nix.
      paths = [ bash coreutils findutils paper-jre ];
      pathsToLink = [ "/bin" ];
    })
    oci-common.passwd
    oci-common.group
    purpurHome
    agent
    (oci-common.binIn { package = spawnery-slp; name = "spawnery-slp"; })
    (oci-common.binIn { package = spawnery-config; name = "spawnery-config"; })
    (oci-common.entrypointFrom ../image/entrypoint.sh)
  ];

  config = {
    # SPAWNERY_PAPER_HOME keeps its name while the entrypoint is shared.
    Env = [
      "HOME=/data"
      "PATH=/bin:/usr/local/bin"
      "SPAWNERY_PAPER_HOME=/opt/purpur"
      "SPAWNERY_SERVER_JAR=/opt/purpur/purpur.jar"
    ];
    ExposedPorts = { "25565/tcp" = { }; };
    Entrypoint = [ "/usr/local/bin/spawnery-entrypoint" ];
    Labels = {
      "org.opencontainers.image.title" = "Spawnery Purpur base image";
      "org.opencontainers.image.version" = "${purpur.purpurVersion}-${imageVersion}";
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
      "cloud.spawnery.purpur-build" = purpur.purpurBuild;
    };
  };
}
