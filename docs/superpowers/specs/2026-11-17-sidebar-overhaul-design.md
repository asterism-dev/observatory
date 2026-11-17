# Sidebar overhaul

## Intent

Replace the workspace rail + side panel with a single sidebar with the user card at the bottom. The main navigation is reserved for the product (Dashboard, Nodes, VMs); org administration, platform administration and the user's own settings move into a popover behind the user card.

## Goals

- One sidebar: app name on top, icon navigation, user card (name + active org) at the bottom.
- Navigation, all org-scoped and empty placeholder pages for now: `/orgs/:orgId/dashboard`, `/orgs/:orgId/nodes`, `/orgs/:orgId/vms`.
- `/` redirects to the active org's dashboard (to `/profile` when the user has no org).
- User card popover:
  - org list, check mark on the active org; switching keeps the current sub-path as today
  - New organization (existing `CreateOrgDialog`)
  - Organization settings — shown when at least one org settings tab is permitted
  - Platform settings — platform admins only
  - Profile
  - Log out
- Organization settings: one layout with tabs General, Members, Teams, Roles, Limits, Audit; each tab gated by `can()` exactly like today's sidebar entries. URLs stay `/orgs/:orgId/...`.
- Platform settings: one layout with tabs Orgs, Users, Audit. URLs stay `/admin/...`.
- Language and theme move from the user menu to the Profile page.
- Mobile keeps working (sidebar collapses to a top bar as today).

## Non-goals

- Search.
- Platform-owned hypervisor hosts (a later platform settings tab).
- Moving the new components into lumen — later, once they have settled.
- Audit resource paths as `org/{org-id}/...` — separate backend PR.

## Approaches

- **A) Own `UserMenu.vue` on lumen `Popover` (picked).** Matches the screenshot, stays in this repo, no lumen release. Keyboard handling is simpler than reka's menu.
- B) Extend lumen `Menu` with separators/groups — reusable, but needs a lumen release first.
- C) Plain lumen `Menu` — fastest, but no dividers/sections.

Picked A; components move to lumen later.

## Decisions

- Settings tabs use lumen `Tabs` rendered by a small layout component wrapping the existing views; existing views stay unchanged apart from removed duplicate headings, if any.
- Org loading/permission logic (`orgId` watch, `switchOrg`, error handling) stays in `AppShell.vue`.
- Icons: add `@lucide/vue`, the icon package lumen itself uses.
- New strings go into `en.json` and `de.json`.
