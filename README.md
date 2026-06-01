# A simple jail for Claude


## TL;DR

Use

	runclaude

instead of `claude`, to protect your home directory against Claude reading it. 

## Why

I love how LLMs speed up writing code, but the security model has me
worried. My machines hold the keys to my professional and private
life, and an LLM going rogue with them is a nightmare scenario. At the
same time, having to babysit a session to approve permission prompts
undoes much of the benefit of agents.

More to the point: I want to run Claude (which uses my AWS account for
inference) with `--dangerously-skip-permissions` without having to
worry that it uses those credentials to touch production systems.


## What

This is a simple sandbox for untrusted processes, in particular,
Claude.

This jail is made with the following assumptions/requirements:

* The user's home directory is full of juicy data (credentials!) which
  the process should not have access to.  This includes the
  credentials for Claude itself.

* Development (including version control, build & test) happens inside
  the jail. This means access to

  - allowlisted directories (eg. source checkout)
  - the host file system (including installed tools)
  - full system call surface (for tests and nested sandboxes)

* The jail should offer minimal friction compared to unsandboxed
  development, to minimize the temptation to run without sandboxing.

  - It should be easy to mix sandboxed and host-based development.

  - The sandbox state should be persistent, so sessions can be
    interrupted and resumed


## How does it work?

The jail uses an unprivileged user namespace, to become root in a
container and then sets up bind-mounts so the host file system (but
not `/home`) is visible. Then it pivots to the new root, dropping
capabilities.

Each sandbox has its own view of `$HOME`, stored under
`~/.cache`. This means that development caches (bazel cache, go module
cache, etc.)  survive across sessions.

The UID inside the container is the same as your own, so the file
system looks the same inside and outside the container.

The container runs in a network namespace, forcing all outgoing
traffic through an HTTPS proxy. Credentials for Claude are injected in
outgoing traffic, so Claude itself doesn't have access to the
credential. The proxy supports both Anthropic bearer credentials and
AWS authentication for use with bedrock.

It uses unprivileged user namespaces, so it does not need root setuid.
It has no runtime dependencies and starts up in ~20ms.


## What conveniences does it offer?

* Repositories underlying Git worktrees/jj workspaces are mapped into
  the container automatically

* Entries from `$PATH` are mapped into the container automatically

* Claude automatically runs with `--dangerously-skip-permissions`, as it
  runs in a sandbox anyway.

* Credentials refresh automatically (both for AWS and Anthropic bearer
  tokens.)

## Standalone proxy (`runproxy`)

The credential-injecting MITM proxy is also available as a standalone
binary for sandboxes that aren't runclaude — for example, a Docker
container. See [`cmd/runproxy/README.md`](cmd/runproxy/README.md) for
the build, the Docker integration recipe, and how to lock down
container egress so the proxy is the only reachable host.

## Limitations

* Linux-only

* The container is setup without `newuidmap`, so anything that needs
  multiple UIDs (eg. rootless podman) can't work

* Some nested sandboxing tools (eg. snap) don't work.

* Networking tools that pin certificates may refuse to work with the
  HTTPS proxy.

## Comparisons

None of the solutions below support MITM credential injection out of
the box.

### Claude's own /sandbox.

The Claude sandbox only applies to subcommands. It happily complies
with "Show me the contents of ~/.credential", meaning my credentials
may go to Anthropic servers, or any other network destinations that
Claude thinks are safe enough.

### Namespace based on Linux

This is a popular approach (eg. sandstorm.io, bubblewrap, firejail).

The security profile is similar to `runclaude`, and can be made
stronger using seccomp.

Most of these tools are written in unsafe languages: Bubblewrap and
Firejail are written in C, and Sandstorm in C++. To top it, Firejail is
setuid root.

### VM

VMs provides a strong isolation boundary between the untrusted
workload and the host. However, the isolation comes at a price:

   * starting a VM is comparatively slow
   * sharing data between host and guest is cumbersome.
   * not all Cloud machines support (nested) virtualization

### Docker/podman

These combine the disadvantages of VMs and namespace jails: you get
the weaker security boundary of namespaces, and the clumsy host/guest
boundary resulting from completely separating the guest file system.

## How secure is it?

On Fedora 44, Claude (4.7 Opus / medium) could not break out without
trying kernel exploits.

It is assumed that the Linux kernel and OS distribution setup prevent
local privilege escalation. With a kernel vulnerability, all bets are
off.

## Licensing

Apache v2.

