# Custom worker images

The published worker image ships four variants (see the README): the default
`full` (Go, Node, Python, Rust) and the slim `go-node`, `python`, and `rust`
variants. Together they cover the toolchains ContextMatrix builds and tests
against directly. **Any other ecosystem — a JVM, Ruby, .NET, a system library a
crate links against — needs a custom image that carries that toolchain.**

A custom image is the published worker image plus your toolchain. Build `FROM`
one of the published variants so the agent binary, entrypoint, unprivileged
`user` (UID 1000), and baseline CLIs (`git`, `gh`, `rg`, `fd`) are inherited
unchanged — then `apt-get install` (or otherwise add) what your project needs.

## Worked example — add a JDK

```dockerfile
# Start from a published variant. Pick the slimmest one that still carries the
# language toolchains your project uses; use full if you need several.
FROM ghcr.io/mhersson/contextmatrix-agent:go-node

# Everything below runs as root; the base image switches back to USER user via
# its ENTRYPOINT, so do not re-declare USER/ENTRYPOINT unless you must.
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

The same shape works for any apt-installable toolchain — for example Ruby is a
one-liner (`apt-get install -y --no-install-recommends ruby-full`) in place of
the Temurin block.

## Keep these intact

- **Do not** override `ENTRYPOINT` — the base image runs `contextmatrix-agent
  work`, which is what ContextMatrix launches.
- **Do not** remove or relocate `/usr/local/bin/contextmatrix-agent`.
- **Do not** change UID 1000 / the `user` account, or the harness loses its
  home and write permissions. End your Dockerfile on `USER user`.
- Add your apt repo keyrings under `/usr/share/keyrings` and clean
  `/var/lib/apt/lists/*` in the same layer, matching the base image's pattern.

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
push under your own name — `ghcr.io/you/my-worker` — add a matching substring
(e.g. `my-worker`) to `image_list_filters` in `serve.yaml`, or the dropdown
will not list it even though `worker_image` still works when set directly.
