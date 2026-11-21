## v0.18.0 (2026-11-21)

### Feat

- **telemetry**: VM metrics, logs and processes (#39)

## v0.17.0 (2026-11-20)

### Feat

- **telemetry**: node metrics and logs over OTLP (#38)

## v0.16.0 (2026-11-20)

### Feat

- **integrations**: gcloud integration via egress placeholders (#37)

## v0.15.0 (2026-11-20)

### Feat

- **integrations**: several Claude accounts with limits and switching (#36)

## v0.14.0 (2026-11-20)

### Feat

- **vms**: delete-after and deleted VM history (#35)

## v0.13.0 (2026-11-20)

### Feat

- **registry**: hub-run garbage collection at a configurable time (#33)

## v0.12.1 (2026-11-20)

### Fix

- **registry**: token realm on the hub URL the client used (#32)

## v0.12.0 (2026-11-20)

### Feat

- **builds**: configurable builder VM size (#31)

## v0.11.0 (2026-11-19)

### Feat

- **images**: automatic rebuilds on a schedule and on base image changes (#30)

## v0.10.0 (2026-11-19)

### Feat

- **hub**: hub url for nodes and plain-http aware image pulls (#29)

## v0.9.0 (2026-11-19)

### Feat

- **init**: sftp in the guest ssh server (#28)

## v0.8.1 (2026-11-19)

### Fix

- boot step panics and fork/disk name checks (#27)

## v0.8.0 (2026-11-19)

### Feat

- **images**: images as an org resource with registry, builds and defaults (#26)

## v0.7.1 (2026-11-19)

### Fix

- **deploy**: retry guest image downloads on transient errors (#25)

## v0.7.0 (2026-11-19)

### Feat

- **node**: go guest runtime booting oci images (#24)

## v0.6.0 (2026-11-19)

### Feat

- **web**: show the hub version in the sidebar (#23)

## v0.5.0 (2026-11-19)

### Feat

- update nodes from the hub (#22)

## v0.4.0 (2026-11-19)

### Feat

- release version checks and cli self-update (#21)

## v0.3.0 (2026-11-19)

### Feat

- **web**: shared ui components and consistent styling for all pages (#20)

## v0.2.0 (2026-11-19)

### Feat

- **web**: rebuild VM and node pages with live disk usage (#18)

## v0.1.0 (2026-11-19)

### Feat

- **web**: dashboard with getting-started guide, resource tiles and docs link (#14)
- **cli**: control vms, connect and manage the org from the terminal (#13)
- guest image with preinstalled tools, desktop and browser (#12)
- **integrations**: github, claude and linear with proxy or in-vm delivery (#11)
- **egress**: host-bound secrets via a per-vm egress proxy (#10)
- **vms**: pause, hibernate, snapshots, fork, auto-suspend and fast boot (#9)
- add vm disks and storage server (#8)
- add vm network, devices and gateway (#7)
- add nodes and firecracker vms (#6)
- add environment variables and secrets (#5)
- **web**: replace workspace rail with sidebar and user menu (#4)
- add orgs, teams, custom roles and limits with casbin (#3)
- add login, personal orgs and audit foundation (#1)
