# `runproxy` — standalone credential-injecting MITM proxy

The credential-injecting MITM proxy that backs `runclaude` is also
available as a standalone binary, for sandboxes that aren't
runclaude — for example, a Docker container.

## Build and run

```
go build -o runproxy ./cmd/runproxy
runproxy \
  --listen 127.0.0.1:8443 \
  --export-bundle ./ca-bundle.crt \
  --anthropic                          # and/or --bedrock
```

The proxy reads Anthropic credentials from
`~/.claude/.credentials.json` (override the lookup root with
`--anthropic-home`) or `$ANTHROPIC_API_KEY`. With `--bedrock` it uses
the default AWS SDK credential chain (SSO, profile, IMDS — same as
runclaude itself).

## Point a container at it

```
docker run \
  -e HTTPS_PROXY=http://host.docker.internal:8443 \
  -e NODE_EXTRA_CA_CERTS=/etc/runproxy/ca-bundle.crt \
  -e SSL_CERT_FILE=/etc/runproxy/ca-bundle.crt \
  -v $PWD/ca-bundle.crt:/etc/runproxy/ca-bundle.crt:ro \
  your-image
```

The container's claude (or any other client) sees only the stub
credentials its image was built with; the proxy strips them and signs
each request with the real credentials before forwarding. The
container itself never has access to the secrets.

## Locking down container egress

Docker by itself won't enforce "only use the proxy" — `HTTPS_PROXY`
is advisory, and a misbehaving process can just open its own socket.
To actually constrain egress, the container needs to be on a network
that has no route to the internet, and the proxy needs to be the only
thing reachable on it. The standard pattern is two networks plus a
sidecar:

```bash
# Egress-allowed network for the proxy only.
docker network create egress

# Internal network: no NAT, no default gateway out. Containers on this
# network can talk to each other but not to anything outside Docker.
docker network create --internal sandbox

# Run the proxy. It needs the egress net (to reach Anthropic/Bedrock) and
# the sandbox net (to be reachable by the app).
docker run -d --name runproxy \
  --network egress \
  -v $PWD/ca-state:/var/lib/runproxy \
  -v $HOME/.claude:/root/.claude:ro \
  myorg/runproxy \
    --listen 0.0.0.0:8443 \
    --ca-dir /var/lib/runproxy \
    --export-bundle /var/lib/runproxy/ca-bundle.crt \
    --anthropic --anthropic-home /root
docker network connect sandbox runproxy

# Run the sandboxed app. Only sees the sandbox network -> only reachable
# host is "runproxy". Even raw socket attempts go nowhere.
docker run --rm -it \
  --network sandbox \
  --dns 127.0.0.11 \
  -e HTTPS_PROXY=http://runproxy:8443 \
  -e HTTP_PROXY=http://runproxy:8443 \
  -e NODE_EXTRA_CA_CERTS=/etc/runproxy/ca-bundle.crt \
  -e SSL_CERT_FILE=/etc/runproxy/ca-bundle.crt \
  -v $PWD/ca-state/ca-bundle.crt:/etc/runproxy/ca-bundle.crt:ro \
  your-image
```

The two-network split is the load-bearing part:

- `--internal` on `sandbox` strips the default gateway in that
  network's bridge, so traffic destined for the internet has nowhere
  to go — even with a malicious binary doing `connect(2)` directly.
- The proxy container is dual-homed, so it forwards on the app's
  behalf and applies its own allowlist (`proxy.MatchDomain` against
  `Allowed`/`Mitm`).
- DNS inside the sandbox resolves only what Docker's embedded
  resolver knows (other containers on `sandbox`, i.e. `runproxy`).
  Anything else NXDOMAIN's.

Hardening you may want on top:

- `--cap-drop=ALL --security-opt=no-new-privileges` on the app
  container so even a kernel-cap escape can't `iptables -F` its way
  out.
- Block the cloud metadata service explicitly if you're on
  EC2/GCP/Azure: `--add-host=metadata.google.internal:127.0.0.1`
  etc., since SSRF through the proxy is the remaining exfil channel.
- If you want the app to reach a *specific* extra host (say
  `github.com`), don't poke a hole in Docker — add it to
  `--allow-domain github.com` on `runproxy` so the proxy passes it
  through. That keeps the allowlist in one place.

The `--listen 0.0.0.0:8443` is safe here precisely because `egress`
is a private bridge that nothing outside Docker can route to. If you
instead run the proxy directly on the host, keep it on `127.0.0.1`
and either use `--network host` for the app or publish the port only
on `127.0.0.1`.

## Security note

The proxy holds your real Anthropic / AWS credentials; anything that
can connect to its listen address can exfiltrate them through it.
Bind to `127.0.0.1` (the default) and rely on Docker's userland proxy
to expose it to the container, or use a unix socket. Do not bind
`0.0.0.0` on a shared host.

## Embedding

The same `proxy` and `creds` Go packages back both `runclaude` and
`runproxy`, so embedding the MITM into another tool is just a matter
of constructing a `proxy.Setup` and calling `proxy.Serve`.
