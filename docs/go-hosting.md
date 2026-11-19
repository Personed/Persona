# Go hosting

The Go hosting package is the service and installer boundary. It does not replace
the TypeScript service or desktop supervisor on its own.

- `hosting.LoadConfig` reads `$PERSONA_CONFIG` or `persona.config.json` in the
  working directory. `PERSONA_HOST`, `PERSONA_PORT`, `PERSONA_DATA_DIR`,
  `PERSONA_STATIC_DIR`, `PERSONA_DEPLOYMENT`, and `PERSONA_PUBLIC_URL` override
  JSON. Relative paths resolve from the config file. With no explicit static
  path, the loader resolves `apps/web/dist` from that directory.
- A runtime acquires `hosting.AcquireDataLease` before opening stores and
  releases it only after closing writers. The lease interoperates with the
  TypeScript service's `runtime.sqlite` lock.
- `hosting.OpenCredentials` uses the existing `hosting.sqlite` and 32-byte
  `credentials.key`. Use `Ref{Scope: "provider", Name: providerID}` for a legacy
  provider and `Ref{Scope: "mcp", Name: legacyID}` for a managed MCP ID beginning
  with `persona-mcp:`. Their `Secret` is the full original JSON object, not just
  a token string. Do not regenerate a missing key; recover it with the matching
  database from a backup.

## Backup and upgrade

`hosting.BackupInstallation` captures the effective config, the local data
directory (including the database and key), and config-referenced desktop,
controller, and SSH credential files. It creates a standard gzip-compressed tar
archive encrypted with passphrase-based [age](https://age-encryption.org/),
never writes a plaintext archive, and refuses to overwrite an existing output.
Keep the passphrase in a separate password manager and keep recovery archives
outside the data directory.

`hosting.RestoreInstallation` stages and authenticates all bytes privately,
validates the profile config and credentials, and publishes a new profile
directory. Point the runtime at the restored config and clear any environment
overrides that still point at the old installation. Installation binaries,
external web assets, separately managed controller configuration, and remote
work-computer volumes are not in this archive. Continue using the TypeScript
restore path for existing `PERSONA001` archives and separately protect remote
volumes before an upgrade.

`hosting.UpgradeInstallation` takes three supervisor callbacks: stop and drain
writers, install, and verify. After stopping writers it takes the lease and
creates the recovery backup before installing. Install and verify run offline
under the lease; the supervisor may start writers after success. An install or
verify failure keeps the pre-upgrade archive for recovery, not automatic rollback.
