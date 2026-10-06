# docker compose serve

<!---MARKER_GEN_START-->
Serve a Compose HTTP API over a unix socket or TCP port

### Options

| Name                  | Type          | Default                            | Description                                                     |
|:----------------------|:--------------|:-----------------------------------|:----------------------------------------------------------------|
| `--allowed-origins`   | `stringArray` |                                    | Allowed CORS origins; repeat the flag to allow multiple origins |
| `--dry-run`           | `bool`        |                                    | Execute command in dry run mode                                 |
| `--exclude`           | `stringArray` | `[.gocache,node_modules,.jj,.git]` | Directory names to exclude while crawling                       |
| `--guest-stacks-path` | `string`      |                                    | Translate stack bind sources to this guest directory            |
| `--max-depth`         | `int`         | `4`                                | Maximum directory depth to crawl for Compose files              |
| `-p`, `--port`        | `int`         | `0`                                | Listen on the given TCP port instead of a unix socket           |
| `--socket`            | `string`      |                                    | Unix socket path (defaults to DIR/compose.sock)                 |


<!---MARKER_GEN_END-->

With `--guest-stacks-path`, project paths must resolve inside the served folder,
including absolute paths and symlink targets. Default Compose files are selected
only from the requested directory; the server does not search its parents.
Existing containers whose configuration labels use an alias of the same files
retain their ownership.

Compose configurations remain trusted host input. Their env files, includes and
build contexts use Compose's normal host access; restricting the initial project
path and guest bind sources does not sandbox those resource reads.
