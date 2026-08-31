# The Purpur base image.
#
# Purpur is a fork of Paper, and this is nix/paper-image.nix with the jar
# swapped: same entrypoint, same agent, same spawnery-slp and spawnery-config,
# same JRE. What the podspec prescribes is satisfied here for the same reasons
# it is there -- /usr/local/bin/spawnery-slp, port 25565, working directory
# /data, scratch /tmp, a numeric user, and nothing else writable.
#
# It is a separate file rather than paper-image.nix parameterised over a
# flavour, because the two are meant to diverge: this one is where the backend
# image is going and paper-image.nix is being deprecated, so a shared function
# would have to be unpicked again on the way out rather than deleted.
{ bash
, buildEnv
, coreutils
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

  # Its own layer, for the reason paper-image.nix gives: it changes on every
  # commit while the JRE and the patched repo above it do not.
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
      # paper-jre, and not a purpur-jre.nix beside it. The list in
      # nix/paper-jre.nix is a jdeps measurement, so it was re-derived over
      # Purpur's own classpath on 2026-08-31 rather than assumed to carry
      # over: 109 jars against Paper's 105, empty stderr, and the answer was
      # Paper's thirteen exactly. jdk.zipfs is the fourteenth for the same
      # reason it is there -- paperclip's FileSystems.newFileSystem reaches it
      # through ServiceLoader, which no static analysis can follow.
      #
      # A Purpur bump wants that measurement repeated, exactly as a Paper bump
      # does, and `make purpur-image-test` is what catches getting it wrong:
      # loudly, in a container, rather than in this file.
      paths = [ bash coreutils paper-jre ];
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
    # PAPER_HOME keeps its name while the entrypoint is shared, and points at
    # this image's own directory. SPAWNERY_SERVER_JAR is what makes the shared
    # script exec Purpur; see image/entrypoint.sh for why the variable is
    # SPAWNERY_-prefixed.
    Env = [
      "HOME=/data"
      "PATH=/bin:/usr/local/bin"
      "PAPER_HOME=/opt/purpur"
      "SPAWNERY_SERVER_JAR=/opt/purpur/purpur.jar"
    ];
    ExposedPorts = { "25565/tcp" = { }; };
    Entrypoint = [ "/usr/local/bin/spawnery-entrypoint" ];
    Labels = {
      "org.opencontainers.image.title" = "Spawnery Purpur base image";
      "org.opencontainers.image.version" = "${purpur.purpurVersion}-${imageVersion}";
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
      # The build number lives here rather than in the tag, so an upstream
      # rebuild does not force every sample manifest to be touched.
      "cloud.spawnery.purpur-build" = purpur.purpurBuild;
    };
  };
}
