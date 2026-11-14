# Repository checkout (compose-server fork)

`POST /v1.24/repos/checkout` clones or reuses an HTTPS repository in the stacks directory.
The request is unchanged for public and private GitHub repositories:

```json
{"url":"https://github.com/owner/repository.git","path":"team","depth":100}
```

`path` is an optional prefix with up to three components. Without it, the
repository is placed directly under the stacks root in its own named directory.
`depth` defaults to 100 and must be between 1 and 100.

`branch` optionally selects a branch on the first clone. `sparsePaths` optionally
lists repository-relative folders to materialize using Git cone-mode sparse
checkout. Root files are included by Git. For example:

```json
{"url":"https://github.com/Agent54/xe-harnesses","branch":"main","sparsePaths":["codex"]}
```

Sending the same URL and parent `path` with `"sparsePaths":["pi"]` reuses the
checkout and adds Pi without removing Codex or local edits. Requests for the
same destination are serialized. Existing full checkouts stay full. Omitting
`sparsePaths` or sending an empty list expands an existing sparse checkout to
the whole repository. Reuse checks the origin URL and requested branch; a
different repository or branch produces HTTP 409 without switching or resetting
the checkout. Non-repository directories and symlink destinations are rejected.
Folder names use the same safe components as `path`, without its three-level
depth limit. New sparse clones request blob filtering and defer checkout until
the folders have been selected.

Checkout parents may contain unrelated files, sockets, symlinks, and sibling
projects. The actual parent path must contain only directories, with no
symlinks. An existing destination must be a matching Git repository with its own
`.git` directory.

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
