# The class path and main class the bundler would use, read from its own
# manifests, so the entrypoint can start the server without the bundler. The
# server jar from versions/ comes first: behind the libraries, Mojang's logging
# library shadows the server's patched LogUtils (NoSuchMethodError).
{ runCommand, unzip }:

{ name, serverJar, repo, home }:

runCommand "${name}-launch" { nativeBuildInputs = [ unzip ]; } ''
  mkdir -p $out${home}
  : > entries
  for dir in versions libraries; do
    unzip -p ${serverJar} META-INF/$dir.list > $dir.list
    while IFS=$'\t' read -r _ _ path; do
      if [ ! -f "${repo}/$dir/$path" ]; then
        echo "META-INF/$dir.list names $path, which ${repo}/$dir does not carry" >&2
        exit 1
      fi
      echo "${home}/repo/$dir/$path" >> entries
    done < $dir.list
  done
  paste -sd: entries > $out${home}/launch.classpath
  unzip -p ${serverJar} META-INF/main-class > $out${home}/launch.main
  test -s $out${home}/launch.main
  chmod -R a-w $out${home}
''
