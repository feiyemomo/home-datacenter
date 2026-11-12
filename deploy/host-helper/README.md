# Home Datacenter Host Helper Daemon

## 1. Overview
`home-datacenter-host-helper` is an independent host-side privileged daemon running outside the Docker container namespace. It exposes an authenticated, cryptographically fenced, fail-closed Unix Domain Socket (`/run/home-datacenter/host-bridge.sock`) to execute strictly typed and whitelisted host maintenance actions.

## 2. Key Separation & Threat Boundary
1. **Bridge Transport Key (`OPS_HOST_BRIDGE_HMAC_KEY`)**:
   - Shared between API container and Host Helper.
   - Proves request authenticity, integrity, and anti-replay nonce validity over Unix domain socket.
2. **Independent Grant Authority Key (`OPS_HOST_GRANT_PUBLIC_KEY` / `OPS_HOST_GRANT_HMAC_KEY`)**:
   - **API container DOES NOT hold the Grant private signing key**.
   - Grants must be minted and signed by an external human approval workflow or dedicated isolated authorization authority.
   - Host Helper validates the cryptographic signature, expiration, resource scope, and canonical step parameter digest before executing any mutating action.

## 3. Unix Domain Socket Security & GID Mapping
- **Parent Directory**: `/run/home-datacenter` created with permission `0750` (`root:homedc-bridge`).
- **Socket Permissions**: `0660` (`rw-rw----`).
- **Symlink Protection**: Host Helper inspects paths via `os.Lstat`. If the socket directory or socket path is a symlink or non-socket regular file, Host Helper aborts startup immediately (Fail-Closed).
- **Docker Compose Integration**:
  ```yaml
  services:
    api:
      user: "1000:1000"
      group_add:
        - "998" # GID of homedc-bridge on host
      volumes:
        - /run/home-datacenter/host-bridge.sock:/run/home-datacenter/host-bridge.sock:rw
  ```

## 4. Building and Installation
```bash
# Compile binary on host or builder
cd services/api
go build -ldflags="-s -w" -o /usr/local/bin/home-datacenter-host-helper ./cmd/host-helper

# Install systemd service
cp deploy/host-helper/home-datacenter-host-helper.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now home-datacenter-host-helper
```
