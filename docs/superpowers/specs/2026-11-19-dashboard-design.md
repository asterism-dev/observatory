# Dashboard and docs link

## Intent

The org dashboard (`/orgs/{org}/dashboard`) is the first page after login and is a placeholder today. It should get a
person from zero to an SSH session in a VM, the way an editor's welcome walkthrough does, and once things exist give an
overview of the org's resources. Every page also gets a link to the docs.

## Goals

- A getting-started guide with steps that tick themselves off from real data, each with a button to where it is done.
- Summary tiles for VMs, nodes, disks and devices with their states, each linking to its list.
- A docs button in the header of every page.

## Non-goals

- No progress stored on the server; no new dashboard API.
- No docs site: publishing the docs (GitHub Pages) is a separate decision.
- No charts, history or usage graphs.

## Approaches

1. **Frontend only (chosen).** The dashboard calls the list endpoints that exist (`/api/orgs/{org}/vms`, `nodes`,
   `disks`, `devices`, `/api/me/ssh-keys`) and derives guide and tiles. Permissions come for free: a list the person may
   not read answers 403 and its tile is left out. Five requests per page load, irrelevant at this size.
2. A `GET /api/orgs/{org}/dashboard` summary endpoint: one request, reusable by the CLI, but new backend code that
   repeats the permission checks and returns nothing the lists do not.
3. Server-side guide progress per user: dropped, every step is derivable from data.

User picked layout A: the guide sits on top while steps are open and can be hidden; tiles show below as soon as
anything exists. Docs link: configurable, defaulting to the repo's GitHub Pages.

## Design

Guide steps, in order, each done when:

| Step | Done when | Action |
| --- | --- | --- |
| Add a node | the org has a node | admins (`node:create`): Add node dialog; others: "ask an admin to add a node", no button |
| Add an SSH key | `/api/me/ssh-keys` is not empty | link to Profile |
| Connect a device | one of my devices in this org exists | Devices page, Add device |
| Start a VM | a VM I can see is `running` | VMs page, Create VM |
| Connect over SSH | one of my devices had a handshake and a VM is running | shows `ssh ubuntu@<vm>.internal` with copy button |

- The node step shows only when the org has no node, or always for admins (our call: members cannot act on it, so it
  only appears as the blocker it is).
- "Hide" is remembered per org in `localStorage`; the guide also disappears when every step is done.
- Tiles: VMs (running / stopped / other), nodes (online / offline), disks (count, total GB, attached), devices
  (connected = handshake within 3 minutes, our call: WireGuard re-handshakes every 2 minutes while traffic or keepalives flow; total). A tile whose list returned 403 is not shown. With nothing to show
  (all lists empty) there are no tiles, only the guide.
- Texts in `en.json` and `de.json`, like every view.

Docs link:

- Hub env `OBSERVATORY_DOCS_URL`, default `https://asterism-dev.github.io/observatory/`; `/api/me` returns it as
  `docsURL`. The app header shows a docs button (book icon, opens in a new tab) on every page.

## Left to the builder

Component split, exact tile copy, loading and error states (a failed list other than 403 shows the tile with an error
line, not a broken page).
