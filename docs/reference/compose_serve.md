# docker compose serve

<!---MARKER_GEN_START-->
Serve a Compose HTTP API over a unix socket or TCP port

### Options

| Name                | Type          | Default                            | Description                                                     |
|:--------------------|:--------------|:-----------------------------------|:----------------------------------------------------------------|
| `--allowed-origins` | `stringArray` |                                    | Allowed CORS origins; repeat the flag to allow multiple origins |
| `--dry-run`         | `bool`        |                                    | Execute command in dry run mode                                 |
| `--exclude`         | `stringArray` | `[.gocache,node_modules,.jj,.git]` | Directory names to exclude while crawling                       |
| `--max-depth`       | `int`         | `4`                                | Maximum directory depth to crawl for Compose files              |
| `-p`, `--port`      | `int`         | `0`                                | Listen on the given TCP port instead of a unix socket           |


<!---MARKER_GEN_END-->

