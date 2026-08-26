# Custom worker images

The published worker image ships four variants (see the README): the default
`full` (Go, Node, Python, Rust) and the slim `go-node`, `python`, and `rust`
variants. Together they cover the toolchains ContextMatrix builds and tests
against directly. **Any other ecosystem - a JVM, Ruby, .NET, a system library a
crate links against - needs a custom image that carries that toolchain.**

A custom image is the published worker image plus your toolchain. Build `FROM`
one of the published variants so the agent binary, entrypoint, unprivileged
`user` (UID 1000), and baseline CLIs (`git`, `gh`, `rg`, `fd`) are inherited
unchanged - then `apt-get install` (or otherwise add) what your project needs.

## Worked example - add a JDK

```dockerfile
# Start from a published variant. Pick the slimmest one that still carries the
# language toolchains your project uses; use full if you need several.
FROM ghcr.io/mhersson/contextmatrix-agent:go-node

# Everything below runs as root. USER is image metadata, not a runtime switch:
# end your Dockerfile on `USER user` yourself (and do not re-declare
# ENTRYPOINT).
USER root

# Temurin (Adoptium) JDK from the Adoptium apt repo.
# hadolint ignore=DL3008
RUN curl -fsSL https://packages.adoptium.net/artifactory/api/gpg/key/public \
      -o /usr/share/keyrings/adoptium.asc \
    && echo "deb [signed-by=/usr/share/keyrings/adoptium.asc] https://packages.adoptium.net/artifactory/deb bookworm main" \
       > /etc/apt/sources.list.d/adoptium.list \
    && apt-get update && apt-get install -y --no-install-recommends temurin-21-jdk \
    && rm -rf /var/lib/apt/lists/*

# Hand control back to the base image: it already sets USER user, WORKDIR, and
# ENTRYPOINT ["contextmatrix-agent", "work"]. Re-declare USER only, so the added
# layers do not leave the container running as root.
USER user
```

The same shape works for any apt-installable toolchain - for example Ruby is a
one-liner (`apt-get install -y --no-install-recommends ruby-full`) in place of
the Temurin block.

## Keep these intact

- **Do not** override `ENTRYPOINT` - the base image runs `contextmatrix-agent
  work`, which is what ContextMatrix launches.
- **Do not** remove or relocate `/usr/local/bin/contextmatrix-agent`.
- **Do not** change UID 1000 / the `user` account, or the harness loses its
  home and write permissions. End your Dockerfile on `USER user`.
- Add your apt repo keyrings under `/usr/share/keyrings` and clean
  `/var/lib/apt/lists/*` in the same layer, matching the base image's pattern.
- Re-declare `CMX_READ_ONLY_ROOTS` when you add a toolchain. It is the
  colon-separated list of absolute trees the agent's `read`, `grep` and `glob`
  tools may resolve outside the workspace, so the shell-less planning,
  diagnosis, review and checkpoint phases can read dependency source instead of
  guessing at an API. Your image inherits the base stage's value, which names
  only that stage's toolchains, and setting it **replaces** the list rather than
  adding to it - repeat the inherited paths alongside your own. Three rules:
  - Use absolute paths, and `mkdir -p` each one in the image. A root that does
    not exist when the worker starts is dropped silently, and the phases then
    behave exactly as they did before, with nothing in the run to say why.
  - Point at where the toolchain writes *at runtime*. The agent scrubs the
    environment of every subprocess it runs for the model down to `PATH`,
    `HOME`, `USER`, `LANG`, `LC_ALL`, `TMPDIR` and `TERM`, so a cache-prefix
    variable you set with `ENV` does not reach the tool and it falls back to its
    own default under `$HOME`. A JDK worker declares `/home/user/.m2/repository`
    for the same reason the Rust variant declares `/home/user/.cargo/registry`.
    If the toolchain must see that variable, name it in a project's
    `verify.env`, which is the allowlist the scrub honours; the value itself
    still has to be in the container, from the image or `worker_extra_env`.
  - Declare only source trees. Writes are never widened: `edit`, `write` and
    `bash` stay inside the workspace whatever this is set to.

## Publish and point a project at it

Build, push, and **pin by digest** so runs are reproducible:

```bash
docker build -t ghcr.io/you/my-worker:jdk .
docker push ghcr.io/you/my-worker:jdk
docker buildx imagetools inspect ghcr.io/you/my-worker:jdk   # copy the index digest
```

Then set the project's `remote_execution.worker_image` on its board to
`ghcr.io/you/my-worker@sha256:<digest>`. Cards for that project launch in your
custom image; every other project keeps using the published default.

A custom image only appears in the ContextMatrix settings dropdown when one of
the backend's `image_list_filters` substrings matches one of its tags (see
`serve.yaml.example`; the default is `[contextmatrix-agent]`). If you tag and
push under your own name - `ghcr.io/you/my-worker` - add a matching substring
(e.g. `my-worker`) to `image_list_filters` in `serve.yaml`, or the dropdown
will not list it even though `worker_image` still works when set directly.
