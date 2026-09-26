# Repository checkout (compose-server fork)

`POST /v1.24/repos/checkout` clones an HTTPS repository into the stacks directory.
The request is unchanged for public and private GitHub repositories:

```json
{"url":"https://github.com/owner/repository.git","path":"team","depth":100}
```

`path` is an optional prefix with up to three components. Without it, the
repository is placed directly under the stacks root in its own named directory.
`depth` defaults to 100 and must be between 1 and 100.

## Private GitHub repositories

Install Git and GitHub CLI (`gh`) on the host running compose-server. Sign in once
as the same OS user that runs the server or launcher:

```sh
gh auth login --hostname github.com --git-protocol https
```

The selected GitHub account must have read access to the repository, including
any organization SSO authorization. For headless deployments, GitHub CLI also
accepts `GH_TOKEN` from the server's environment. Do not include a token in the
repository URL or API request.

The server uses `gh auth git-credential` only for HTTPS `github.com` requests on
the default HTTPS port. The helper reads GitHub CLI's saved credentials; the
server does not save the token in the checkout, URL, or Git configuration. The
launcher must be able to find `gh` on its `PATH`. `gh auth setup-git` is not needed.

Public repositories still work without GitHub CLI. Other hosts and GitHub
Enterprise hosts currently support anonymous HTTPS checkout only. SSH URLs and
URLs containing credentials are rejected.

Terminal and askpass prompts remain disabled. The server still ignores global
and system Git configuration, including URL rewrites and arbitrary credential
helpers. Authentication does not require changing the checkout API or UI.
