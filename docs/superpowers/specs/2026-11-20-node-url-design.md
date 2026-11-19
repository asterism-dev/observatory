# Hub URL for nodes (image pulls)

## Intent
Nodes pull user images through the hub's `/v2/` proxy at `<registry host>/i/<id>`, where the host is taken from `OBSERVATORY_PUBLIC_URL`. In deployments where browsers use a different address than nodes (e.g. a hub on a laptop at `http://localhost:8090` with nodes reaching it over a private overlay network at `http://100.x.y.z:8090`), nodes cannot pull user images at all. Additionally go-containerregistry only uses plain HTTP for loopback/RFC 1918 hosts, so an `http://` hub on another private range (100.64.0.0/10) fails with a TLS error.

## Design (our calls)
- New hub setting `OBSERVATORY_NODE_URL` (optional): the hub URL nodes reach. Default: `OBSERVATORY_PUBLIC_URL`. The node image repo host (`internal/fleet` registryHost) uses it.
- `proto.ImageRef` gains `PlainHTTP bool` (json `plainHTTP,omitempty`), set by the hub when the node URL's scheme is `http`; the node's ociimage pull then uses `name.Insecure` for that reference (and only then). Builder VMs: `OBSERVATORY_BUILDER_HUB_URL` already exists; when it is `http`, the build agent must configure BuildKit for a plain-HTTP registry for that host (check what the agent does today and make it explicit + tested).
- Docs: deploy.md documents `OBSERVATORY_NODE_URL` (when browsers and nodes use different addresses) and recommends https for public hubs; plain http only on trusted private networks (overlay/VPN).
## Tests
fleet: repo host from node URL; PlainHTTP set iff node URL is http. node/ociimage: PlainHTTP pulls over http from a test registry on a non-RFC1918-looking host name (use httptest + name.Insecure path), and https otherwise. buildagent: http hub → BuildKit registry config marks the host http.
