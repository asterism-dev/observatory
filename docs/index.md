# Observatory

Observatory is a hub for teams that work in cloud VMs. It manages organisations, teams and permissions, runs VMs on
Firecracker nodes you add yourself, and connects them over a private network: every person reaches their VMs from
their own machine through WireGuard, with `ssh dev@<vm>.internal`.

- **VMs** with pause, hibernate, snapshots, forks and auto-suspend, a standard image with the usual developer tools (or any OCI image), and
  an optional desktop and browser.
- **Disks** that outlive their VM, **networks** between VMs, and **devices** that connect people to them.
- **Secrets** that never enter a VM in plain form: an egress proxy adds them to outgoing requests, and integrations for
  GitHub, Claude and Linear build on it.
- A **web app**, a **CLI** (`observatory`) for everything the web app does, and an append-only audit log.

## Where to start

| You want to | Read |
| --- | --- |
| run a hub on a server | [Deploy with Docker Compose](deploy.md) and [Identity provider](idp.md) |
| add machines that run VMs | [Nodes](nodes.md) |
| work with VMs from the terminal | [CLI](cli.md) |
| understand how VMs reach each other and you | [Network](network.md) |

The first person to log in to a new hub becomes its platform admin. Releases, binaries and container images are on
the [releases page](https://github.com/asterism-dev/observatory/releases).
