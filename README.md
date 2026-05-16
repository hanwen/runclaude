# A simple jail for Claude

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

* The jail should offer minimal friction, so it is feasible to run
  development sessions in the jail by default. This means:

  - It should be easy to mix sandboxed and host-based development.

  - The sandbox state should be persistent, so sessions can be
    interrupted and resumed


# How does it work?

The jail uses a user namespace to become root.  It sets up bind-mounts
so the host file system (but not /home) is visible, and then pivots to
the new root, dropping capabilities.

Each sandbox has its own view of $HOME, stored under ~/.cache. This
means that development caches (bazel cache, go module cache, etc.)
survive across sessions.

The UID inside the container is the same as your own, so all files
that end up in your source checkout are owned by you.

The container runs in a network namespace, forcing all outgoing
traffic through an HTTPS proxy. Credentials for Claude are injected in
outgoing traffic, so Claude itself doesn't have access to the
credential.


# How secure is it?

On Fedora 44, Claude (4.7 Opus / medium) could not break out without
trying kernel exploits.

It is assumed that the Linux kernel and OS distribution setup prevents
local privilege escalation. With a kernel vulnerability, all bets are
off.

# Licensing

Apache v2.

