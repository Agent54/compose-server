# Repository checkout (compose-server fork)

`POST /v1.24/repos/checkout` clones an HTTPS repository into the stacks directory.
The request is unchanged for public and private GitHub repositories:

```json
{"url":"https://github.com/owner/repository.git","path":"team","depth":100}
```

`path` is an optional prefix with up to three components. Without it, the
repository is placed directly under the stacks root in its own named directory.
`depth` defaults to 100 and must be between 1 and 100.

Checkout parents may contain unrelated files, sockets, symlinks, and sibling
projects. The actual parent path must contain only directories, with no
symlinks, and the repository destination must not already exist.

To prevent accidental nesting, the server checks the serve root and each parent
component for `.git` (including Git worktree files) and the default Compose and
override filenames used by discovery. A project marker causes an HTTP 409
response identifying the parent and marker. Nested checkout is always rejected;
there is no override. Choose a grouping directory outside the existing project.
The guard checks ancestors within the serve root, rather than files inside the
cloned repository.

## Private HTTPS repositories

Checkout uses the normal Git configuration of the OS user running
compose-server, including system and global credential helpers and inherited
Git environment settings. Configure that user's Git credentials and confirm
they can clone the repository with its HTTPS URL before using the API. This
works for GitHub and other HTTPS hosts without requiring GitHub CLI or a token
field in the request. Do not include credentials in the URL.

Terminal prompts are disabled because the API request cannot answer them.
Configured credential helpers and askpass programs remain available. Git still
rejects non-HTTPS transports, including URLs rewritten to SSH by Git config.
The existing HTTPS URL validation and checkout filesystem protections are
unchanged.
