# Locally-trusted TLS certs (mkcert)

This directory holds the per-machine TLS material used by nginx in
the dev `docker-compose.full.yml` stack. The actual `dev.crt` /
`dev.key` files are gitignored -- generate them once on each
developer's machine by running:

```bash
make setup-tls
```

(or directly: `bash scripts/dev/setup-tls.sh`).

The script wraps [mkcert](https://github.com/FiloSottile/mkcert),
which:

  1. Generates a per-machine root CA (the first run -- subsequent
     runs reuse the same CA).
  2. Installs that CA into the system trust store, the Firefox
     trust store, and the Java truststore (if present), so
     browsers, Go clients, and the cockpit-worker all trust the
     resulting certs without warnings.
  3. Issues a leaf cert with one wildcard SAN covering every dev
     hostname slot: `*.${IDENTITY_BOOTSTRAP_DOMAIN}` (default
     `*.local.znas.io`). The script reads
     `IDENTITY_BOOTSTRAP_DOMAIN` from the env or `.env.local`;
     export it to issue the cert for a different parent domain.

After generation, `docker compose -f docker/docker-compose.full.yml
restart nginx` for nginx to pick up the new files.

## Prerequisites

* macOS: `brew install mkcert nss`
* Linux: see https://github.com/FiloSottile/mkcert#installation

## Why these are gitignored

The certs are signed by a CA that lives in your individual user
account -- they only validate from your machine. Committing them
would give every collaborator an invalid cert (the CA chain
wouldn't match their local trust store) and would needlessly leak
the leaf-cert lifetime.
