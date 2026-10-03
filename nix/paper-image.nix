# The Paper base image. Deprecated in favour of nix/purpur-image.nix but still
# built and published: existing ServerGroups name its tag in spec.image, so it
# goes only with a release note.
#
# The pod spec is already written, so this image must provide
# /usr/local/bin/spawnery-slp, port 25565, working directory /data, scratch
# /tmp, a numeric user, and nothing else writable.
{ bash
, buildEnv
, coreutils
, findutils
, paper-jre
, runCommand
, paper
, spawnery-slp
, spawnery-config
, agents
, imageVersion
, oci-common
}:

let
  paperHome = runCommand "paper-home" { } ''
    mkdir -p $out/opt/paper
    cp ${paper.paperJar} $out/opt/paper/paper.jar
    cp -r ${paper.repo} $out/opt/paper/repo
    chmod -R a-w $out/opt/paper
  '';

  # Its own layer: it changes on every commit, the JRE and Paper repo do not.
  agent = runCommand "paper-agent-image-path" { } ''
    mkdir -p $out/opt/paper/agent
    cp ${agents}/share/spawnery/paper/spawnery-agent.jar $out/opt/paper/agent/spawnery-agent.jar
  '';
in
oci-common.layeredImage {
  name = "ghcr.io/spawnery/paper";
  tag = "${paper.paperVersion}-${imageVersion}";

  # Ordered by rate of change.
  contents = [
    (buildEnv {
      name = "paper-tools";
      # findutils for the entrypoint's chmod walk: find -xdev stops at mounts,
      # chmod -R cannot, and coreutils has no find.
      paths = [ bash coreutils findutils paper-jre ];
      pathsToLink = [ "/bin" ];
    })
    oci-common.passwd
    oci-common.group
    paperHome
    agent
    (oci-common.binIn { package = spawnery-slp; name = "spawnery-slp"; })
    (oci-common.binIn { package = spawnery-config; name = "spawnery-config"; })
    (oci-common.entrypointFrom ../image/entrypoint.sh)
  ];

  config = {
    Env = [
      "HOME=/data"
      "PATH=/bin:/usr/local/bin"
      "SPAWNERY_PAPER_HOME=/opt/paper"
    ];
    ExposedPorts = { "25565/tcp" = { }; };
    Entrypoint = [ "/usr/local/bin/spawnery-entrypoint" ];
    Labels = {
      "org.opencontainers.image.title" = "Spawnery Paper base image";
      "org.opencontainers.image.version" = "${paper.paperVersion}-${imageVersion}";
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
      # The build number lives here rather than in the tag, so an upstream rebuild
      # does not touch every sample manifest.
      "cloud.spawnery.paper-build" = paper.paperBuild;
    };
  };
}
