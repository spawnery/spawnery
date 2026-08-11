# The Paper base image.
#
# Everything the podspec prescribes has to be satisfied here, because the pod
# spec is already written: /usr/local/bin/spawnery-slp, port 25565, working
# directory /data, scratch /tmp, a numeric user, and nothing else writable.
{ bash
, buildEnv
, coreutils
, jdk25_headless
, runCommand
, paper
, spawnery-slp
, spawnery-config
, paper-agent
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

  # Its own layer: it changes on every commit, while the JRE and the patched
  # Paper repo above it do not.
  agent = runCommand "paper-agent-image-path" { } ''
    mkdir -p $out/opt/paper/agent
    cp ${paper-agent}/share/spawnery/spawnery-agent.jar $out/opt/paper/agent/spawnery-agent.jar
  '';
in
oci-common.layeredImage {
  name = "ghcr.io/spawnery/paper";
  tag = "${paper.paperVersion}-${imageVersion}";

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
      # coreutils for the mkdir/cp the entrypoint uses to place the agent
      # jar; bash because the entrypoint's shebang points at it. grep is
      # gone with set_property, the only thing that ever used it.
      paths = [ bash coreutils jdk25_headless ];
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
