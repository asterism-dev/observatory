# gcloud integration design

## Intent

A member runs `terraform plan` and `gcloud logging read` inside their VMs under their own Google user account, without
logging in inside the guest. Their Google Workspace enforces SSO with a session length of about a day, so they log in
on their Mac every morning anyway; handing that login to the VMs must be one extra command.

## Goals

- Python (and other SDK) clients that use ADC work too, over REST and gRPC, and send the quota project.

- New integration kind `gcloud` that makes both logins work in the guest: gcloud's own login and Application Default
  Credentials (ADC, used by Terraform and the Google SDKs).
- No real Google credential (refresh token, access token, client secret) ever reaches the guest.
- Daily re-login: `gcloud auth application-default login` on the Mac, then `observatory integration connect gcloud`.

## Non-goals

- Several Google accounts per integration (one for now).
- Service accounts, workload identity, impersonation.
- Installing `gcloud` or `terraform` in the standard image; the user installs them in the VM.
- A GCE metadata server emulation.
- An "Inside the VM" (`vm`) delivery mode for this kind.
- Hub-side Google sign-in (OAuth flow run by the hub).
- Wildcard host bindings for user variables and secrets.
- Guaranteeing `kubectl`/GKE, `docker push` to Artifact Registry, `bq`, `gsutil`; they may work, untested.

## Anti-goals

- Writing into gcloud's internal databases (`credentials.db`, `access_tokens.db`) or its configuration files.
- Overwriting or overriding a login the user made inside the VM (`gcloud auth login`,
  `gcloud auth application-default login`).

## Constraints

- The SSO session ends the refresh token's usefulness after about a day: Google answers the refresh with
  `invalid_grant` (often `invalid_rapt`). The hub cannot keep the login alive beyond that.
- Google APIs live on many `*.googleapis.com` hosts; egress bindings today allow exact host names only
  (`internal/egress/hosts.go`, `docs/egress.md`).
- The VM's CA is limited by X.509 name constraints (`internal/egress/ca.go`); a permitted domain `googleapis.com`
  already covers its subdomains in X.509 semantics, but `covers()` compares exactly.
- Google access tokens live one hour.
- The user's Workspace currently has no context-aware access (device/IP binding); the hub and the node egress
  refresh and call from other addresses than the Mac. If that policy is introduced later, the approach breaks.

## Approaches

1. **Emulated GCE metadata server in the guest** (hub mints access tokens, init serves `169.254.169.254`): works for
   every client without files or env, but needs emulating several endpoints and relies on clients detecting "GCE".
   Dropped once proxy mode was chosen: a stable placeholder needs no fresh tokens in the guest.
2. **Access token via env/file** (`CLOUDSDK_AUTH_ACCESS_TOKEN_FILE`, `GOOGLE_OAUTH_ACCESS_TOKEN`): only gcloud and
   the Terraform provider, and a real one-hour token goes stale in open shells. Dropped.
3. **Real credential files in the guest:** simplest, but a long-lived refresh token in the guest, snapshots and forks.
   Dropped.
4. **Placeholder ADC file + egress proxy (chosen by the user):** the guest holds an `authorized_user` JSON whose
   secrets are placeholders. The proxy answers the client's refresh itself with the placeholder as access token, and
   substitutes the real access token in `Authorization` on `*.googleapis.com`. Every ADC client works; nothing real
   enters the guest. gcloud is pointed at the same file with `CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE` (the user
   rejected writing gcloud's databases).

For connecting, the user chose handing over the local ADC file (they log in locally every morning) over a hub-run
Google sign-in (one command fewer, but depends on gcloud's public OAuth client and means two logins a day).

## Design

### Kind

`gcloud` in `internal/integrations/kinds.go`:

- Source `adc` (an `authorized_user` ADC JSON). Other `type` values, e.g. `service_account`, are refused (400).
- Bound hosts: `*.googleapis.com` (one or more labels; `googleapis.com` itself is not matched). Header:
  `Authorization`.
- Scope: private only (it is a personal SSO login). Org and team scope are refused.
- Mode: `proxy` only; a mode change is refused (400).
- Token page: `https://myaccount.google.com/permissions`.

### Connecting (CLI and API)

- `observatory integration connect gcloud` reads `$CLOUDSDK_CONFIG/application_default_credentials.json` (default
  `~/.config/gcloud/…`) and sends it to the hub. If a `gcloud` integration already exists in the private scope it
  replaces its token (same endpoint as "replace"), so the morning command is always the same. Like other secrets
  input, the content never appears on argv or in logs.
- The hub parses the JSON, keeps `client_id`, `client_secret`, `refresh_token`, `quota_project_id`; verifies by
  refreshing at `https://oauth2.googleapis.com/token`; reads the account email from the token (`tokeninfo` or
  `userinfo`; ADC logins carry the `userinfo.email` scope). Verification failure changes nothing.
- Stored sealed with the org key like Claude's OAuth tokens: refresh token, client id/secret, current access token
  and its expiry.
- **Quota project** (user decision): a plain field of the integration, prefilled from the file's `quota_project_id`
  on connect (unless `--quota-project`/`quotaProject` is given), changeable with
  `PATCH /api/orgs/{org}/integrations/{id}` `{"quotaProject": "…"}` (audited `integration:set`, `action` =
  `quota-project`), the CLI flag and the page. Empty means none. Validated as a GCP project id.

### Hub refresh

- The existing integration refresher refreshes `gcloud` access tokens before expiry. The Claude threshold (60 min
  before expiry) does not fit one-hour tokens; `gcloud` uses its own (builder's call, e.g. 10 min).
- Google does not rotate refresh tokens; the stored one is kept.
- `invalid_grant` (incl. `invalid_rapt`) → status `expired` at once; the page and `observatory integration list`
  say: run `gcloud auth application-default login`, then `observatory integration connect gcloud`. Other errors back
  off like Claude's and retry until the access token expires.
- Each new access token reaches the node's proxy with the next push, like Claude refreshes today.

### Egress: wildcard hosts

- Host validation accepts a leading `*.` only for built-in integration kinds (not via the variables/secrets API).
- The VM CA gets `googleapis.com` as permitted domain; `covers()` and the proxy's "is this host bound" lookup match a
  wildcard entry against any subdomain. Leaves are still issued per concrete host.
- Consequence (documented): the proxy terminates TLS for every `*.googleapis.com` connection of a VM that has the
  integration, including public ones like `fonts.googleapis.com`; only the placeholder in `Authorization` is
  replaced.

### Egress: answering the refresh

- For a VM with a `gcloud` integration, a `POST https://oauth2.googleapis.com/token` with form body
  `grant_type=refresh_token` and `refresh_token` equal to the integration's placeholder is answered by the proxy
  itself, never forwarded: `200`, JSON `{"access_token": <placeholder>, "expires_in": 3600, "token_type": "Bearer",
  "scope": <scopes of the stored login>}`. Any other request to that host passes through with normal substitution.
- When the integration is `expired`, the proxy answers `400 {"error":"invalid_grant"}` so clients show their usual
  "re-authenticate" message instead of a confusing 401 later. (Builder may simplify if it complicates the push.)

### Guest

The integrations sync (`internal/guestinit/integrations.go`) gains `syncGcloud`:

- Writes `~/.config/gcloud/application_default_credentials.json` (0600, as `dev`):
  `{"type":"authorized_user","client_id":<ph>,"client_secret":<ph>,"refresh_token":<ph>,"quota_project_id":…}`
  (`quota_project_id` only when set). Placeholders come from `vmm-token gcloud`; all three fields carry the same
  placeholder. The quota project reaches the guest through the node's token service: its reply gains a `project`
  field next to `token`. Written only if missing or
  still the file the sync wrote (hash bookkeeping, as for `~/.claude/.credentials.json`); removed on disconnect only
  if still ours.
- A `gcloud` wrapper in `/.observatory/bin` (the init binary under another name, like `gh`) runs the user's own
  `gcloud` (found in `PATH` without the platform directory) and sets
  `CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=<home>/.config/gcloud/application_default_credentials.json` only when all
  hold (user decision: a login made in the VM wins):
  - the integration is connected (`vmm-token gcloud` exits 0),
  - the variable is not already set,
  - the active gcloud configuration (`$CLOUDSDK_CONFIG/active_config`, default `default`;
    `configurations/config_<name>`) has no `[core] account`, i.e. no `gcloud auth login` in the VM. Read as INI only;
    gcloud's databases are never read or written,
  - the ADC file is still the one the sync wrote (else the user's own ADC login would be used silently).
  Otherwise it runs `gcloud` unchanged. Nothing is put in `/etc/environment`.
- No `CLOUDSDK_CORE_PROJECT` (our call): it would override `gcloud config set project`, and the quota project is often
  not the working project. The user sets the project with gcloud as usual; `quota_project_id` stays in the ADC file.
- Under the same conditions the wrapper also sets `CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE` to the system bundle that
  contains the VM's CA, when unset: gcloud ignores the system store (spike: `CERTIFICATE_VERIFY_FAILED` without it).
- `gcloud auth list` shows no account with the override; cosmetic, documented.
- `GRPC_DEFAULT_SSL_ROOTS_FILE_PATH` joins the guest's system-wide trust variables (`SSL_CERT_FILE`,
  `REQUESTS_CA_BUNDLE`, …): gRPC ignores the others, and Python's default gRPC clients fail behind the egress without
  it (spike). It is trust configuration for every gRPC client, not a gcloud setting.
- Known gap: programs that run `gcloud` by absolute path bypass the wrapper and get no override.

### Web UI

The Integrations page lists `gcloud` with the account email, status and the morning instructions instead of a paste
dialog (paste of the JSON is fine as a secondary path). Icon: generic cloud.

### Docs

`docs/integrations.md` (kinds table, gcloud section, known limits), `docs/egress.md` (wildcard, synthetic token
response), `docs/cli.md`.

## Spike result (2026-11-20)

Throwaway Docker container (gcloud slim image, Terraform 1.9.8, google provider 8.12.0) behind mitmproxy with its own
CA in the system store, emulating the planned egress; real access token of the user, read-only calls only.

- gcloud accepts the placeholder `authorized_user` file via `CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE`, refreshes at
  `oauth2.googleapis.com/token` (answered by the proxy) and sends `Bearer obsph_…`; `gcloud logging read` → 200.
- gcloud needs `CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE`; without it the refresh fails with `CERTIFICATE_VERIFY_FAILED`.
- Terraform finds the ADC file with no env, refreshes the same way; `terraform plan` (userinfo, project, billing info
  data sources) succeeds.
- Second run with `quota_project_id` in the file: gcloud sends `x-goog-user-project` = its own project (`--project`),
  ignoring the file; Terraform sends none (only with `user_project_override`). Both succeed.
- Python (`google-auth`): REST client works and sends the file's `quota_project_id`; the default gRPC client
  (`logging_v2`) fails with `CERTIFICATE_VERIFY_FAILED` until `GRPC_DEFAULT_SSL_ROOTS_FILE_PATH` points at the system
  bundle, then works over HTTP/2 through the proxy and sends the quota project.
- No placeholder in a URL or request body; every Google call carried it in `Authorization` only. Hosts seen:
  `oauth2`, `logging`, `openidconnect`, `cloudresourcemanager`, `cloudbilling` `.googleapis.com`.

## Known limits

- The hub holds a copy of the Mac's refresh token, not its own: `gcloud auth application-default revoke` (or
  `gcloud auth revoke`) on the Mac also kills the hub's login. A plain new local login does not; the old token stays
  valid until the session ends (Google keeps up to 100 refresh tokens per account and client).
- Team VMs never get `gcloud`: it is private-scope only, and team VMs never receive private integrations.

## Left to the builder

- Refresh threshold and backoff values for `gcloud`.
- How the wildcard entry is represented in the push to the node and in `NormalizeHostSet`.
- Exact error texts, UI layout details, test structure.
