#!/bin/sh
set -eu

dir=$(cd "$(dirname "$0")" && pwd)
hub_env="$dir/../dev/hub.env"
pat_file="$dir/.bootstrap/admin.pat"
api=${ZITADEL_URL:-http://localhost:8080}

# Without arguments: dev setup, writes deploy/dev/hub.env. With --print <hub URL>: registers that hub and prints its OIDC values.
hub_url=''
case ${1:-} in
  '') ;;
  --print)
    [ $# -eq 2 ] || { echo "usage: bootstrap.sh [--print http(s)://HUB]" >&2; exit 2; }
    hub_url=${2%/}
    case $hub_url in http://?* | https://?*) ;; *) echo "hub URL must be http(s)://…, got '$hub_url'" >&2; exit 2 ;; esac
    ;;
  *) echo "usage: bootstrap.sh [--print http(s)://HUB]" >&2; exit 2 ;;
esac

if [ -z "$hub_url" ] && [ -f "$hub_env" ]; then
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

if [ -n "$hub_url" ]; then
  redirects="[\"$hub_url/auth/callback\"]" logouts="[\"$hub_url/\"]"
  # devMode only allows plain-http redirect URIs; https hubs do not need it.
  case $hub_url in http://*) dev=true ;; *) dev=false ;; esac
else
  redirects='["http://localhost:5173/auth/callback","http://localhost:8090/auth/callback"]'
  logouts='["http://localhost:5173/","http://localhost:8090/"]' dev=true
fi

resp=$(post "/management/v1/projects/$project_id/apps/oidc" '{
  "name":"observatory-web",
  "redirectUris":'"$redirects"',
  "postLogoutRedirectUris":'"$logouts"',
  "responseTypes":["OIDC_RESPONSE_TYPE_CODE"],
  "grantTypes":["OIDC_GRANT_TYPE_AUTHORIZATION_CODE"],
  "appType":"OIDC_APP_TYPE_USER_AGENT",
  "authMethodType":"OIDC_AUTH_METHOD_TYPE_NONE",
  "devMode":'"$dev"',
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

if [ -n "$hub_url" ]; then
  printf 'OBSERVATORY_OIDC_ISSUER=%s\nOBSERVATORY_OIDC_WEB_CLIENT_ID=%s\nOBSERVATORY_OIDC_CLI_CLIENT_ID=%s\n' "$api" "$web_id" "$cli_id"
  exit 0
fi

cat > "$hub_env" <<ENV
OBSERVATORY_OIDC_ISSUER=$api
OBSERVATORY_OIDC_WEB_CLIENT_ID=$web_id
OBSERVATORY_OIDC_CLI_CLIENT_ID=$cli_id
OBSERVATORY_DATABASE_URL=postgres://observatory:observatory@localhost:55432/observatory?sslmode=disable
OBSERVATORY_PUBLIC_URL=http://localhost:5173
OBSERVATORY_SECRETS_KEY=$(openssl rand -base64 32)
OBSERVATORY_GATEWAY_TOKEN=$(openssl rand -hex 32)
OBSERVATORY_STORAGE_TOKEN=$(openssl rand -hex 32)
ENV
echo "wrote $hub_env"
