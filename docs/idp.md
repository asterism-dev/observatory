# Identity provider

observatory ships no IdP. It only asks the IdP who someone is; users, orgs,
membership, sessions and audit live in observatory's database. Any OIDC
provider works that offers:

- OIDC discovery (`<issuer>/.well-known/openid-configuration`)
- authorization code flow with PKCE for a public web client
- device authorization grant (RFC 8628) for a public CLI client
- `sub`, `email` and `name` in the ID token or the userinfo endpoint

## Clients

| Client | Type | Grant | Redirect URI |
|--------|------|-------|--------------|
| web | public (no secret) | authorization code + PKCE | `<PUBLIC_URL>/auth/callback` |
| CLI | public (no secret) | device authorization | none |

`<PUBLIC_URL>` is the URL users open in the browser: `http://localhost:5173`
with the Vite dev server, `http://localhost:8090` with only the built hub.

## Hub environment

| Variable | Meaning |
|----------|---------|
| `OBSERVATORY_OIDC_ISSUER` | issuer URL, must be identical for hub and browser |
| `OBSERVATORY_OIDC_WEB_CLIENT_ID` | web client ID |
| `OBSERVATORY_OIDC_CLI_CLIENT_ID` | CLI client ID |
| `OBSERVATORY_PUBLIC_URL` | public URL of the hub, see above |
| `OBSERVATORY_DATABASE_URL` | Postgres connection string |
| `OBSERVATORY_LISTEN` | listen address, default `:8090` |

## Zitadel

`deploy/dev/zitadel/` runs a separate dev Zitadel at `http://localhost:8080`;
`make idp-up idp-bootstrap` starts it, registers both clients and writes
`deploy/dev/hub.env`. For your own Zitadel, create two OIDC applications in a
project: a user-agent app (auth method none, code) and a native app (auth
method none, device code). Enable `idTokenUserinfoAssertion` on both so `email`
and `name` appear in the ID token.

## Keycloak

Create two clients with client authentication off (public). On the web client
keep the standard flow on and add the redirect URI. On the CLI client enable
"OAuth 2.0 Device Authorization Grant". The issuer is
`https://<host>/realms/<realm>`.
