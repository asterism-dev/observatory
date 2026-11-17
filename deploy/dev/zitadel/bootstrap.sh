#!/bin/sh
set -eu

dir=$(cd "$(dirname "$0")" && pwd)
hub_env="$dir/../hub.env"
pat_file="$dir/.bootstrap/admin.pat"
api=http://localhost:8080

if [ -f "$hub_env" ]; then
  echo "$hub_env exists, nothing to do"
  exit 0
fi

i=0
until [ -s "$pat_file" ]; do
  i=$((i + 1))
  [ "$i" -le 60 ] || { echo "timed out waiting for $pat_file" >&2; exit 1; }
  sleep 2
done
pat=$(tr -d '\n' < "$pat_file")

# POST <path> <json>: prints the response body, exits 1 on non-2xx.
post() {
  out=$(mktemp)
  code=$(curl -sS -o "$out" -w '%{http_code}' -X POST "$api$1" \
    -H "Authorization: Bearer $pat" -H 'Content-Type: application/json' -d "$2")
  case "$code" in
    2??) cat "$out"; rm -f "$out" ;;
    *) echo "POST $1 failed ($code):" >&2; cat "$out" >&2; echo >&2; rm -f "$out"; exit 1 ;;
  esac
}

resp=$(post /management/v1/projects '{"name":"observatory"}')
project_id=$(printf %s "$resp" | jq -er .id)

resp=$(post "/management/v1/projects/$project_id/apps/oidc" '{
  "name":"observatory-web",
  "redirectUris":["http://localhost:5173/auth/callback","http://localhost:8090/auth/callback"],
  "postLogoutRedirectUris":["http://localhost:5173/","http://localhost:8090/"],
  "responseTypes":["OIDC_RESPONSE_TYPE_CODE"],
  "grantTypes":["OIDC_GRANT_TYPE_AUTHORIZATION_CODE"],
  "appType":"OIDC_APP_TYPE_USER_AGENT",
  "authMethodType":"OIDC_AUTH_METHOD_TYPE_NONE",
  "devMode":true,
  "idTokenUserinfoAssertion":true}')
web_id=$(printf %s "$resp" | jq -er .clientId)

resp=$(post "/management/v1/projects/$project_id/apps/oidc" '{
  "name":"observatory-cli",
  "responseTypes":["OIDC_RESPONSE_TYPE_CODE"],
  "grantTypes":["OIDC_GRANT_TYPE_DEVICE_CODE"],
  "appType":"OIDC_APP_TYPE_NATIVE",
  "authMethodType":"OIDC_AUTH_METHOD_TYPE_NONE",
  "devMode":true,
  "idTokenUserinfoAssertion":true}')
cli_id=$(printf %s "$resp" | jq -er .clientId)

cat > "$hub_env" <<ENV
OBSERVATORY_OIDC_ISSUER=$api
OBSERVATORY_OIDC_WEB_CLIENT_ID=$web_id
OBSERVATORY_OIDC_CLI_CLIENT_ID=$cli_id
OBSERVATORY_DATABASE_URL=postgres://observatory:observatory@localhost:55432/observatory?sslmode=disable
OBSERVATORY_PUBLIC_URL=http://localhost:5173
OBSERVATORY_SECRETS_KEY=$(openssl rand -base64 32)
OBSERVATORY_GATEWAY_TOKEN=$(openssl rand -hex 32)
ENV
echo "wrote $hub_env"
