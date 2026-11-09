# Hermes AI

This branch adds `routing.strategy: soonest-reset` to CLIProxyAPI. It ranks eligible, same-priority Claude and Codex OAuth credentials by the next weekly reset. The existing affinity selector keeps its bindings. The new selector only makes a choice when the existing selector would select an account, such as for a new session or failover.

Quota reads run outside the request path. A cold cache, missing or stale data, mixed providers, custom transports, or an unrankable pool falls back to the normal round-robin selector. A known exhausted account is skipped when ranking a fully observed pool. The endpoints are provider implementation details and may stop supplying usable quota data. The proxy keeps serving requests if that happens.

## Release procedure

1. Resolve the latest stable release using GitHub's Releases API for `router-for-me/CLIProxyAPI`. Compare its tag with the upstream tag recorded for the deployed build.
2. Rebase only this fork's maintenance commits onto the stable tag, using the previously recorded upstream tag as the old base. Abort and report on conflicts. Run the full Go tests and compile the server.
3. Build the official Dockerfile for Linux amd64. Use an immutable local tag `hermes-ai/cliproxyapi:<upstream-version>-<fork-commit>` and label the build with the exact upstream tag and fork commit. Transfer the image over the VM's existing SSH connection.
4. Run `sudo bash -s -- <image-tag>` on the dedicated VM with `ops/hermes/deploy.sh` as stdin. It tests the candidate in an isolated container, recreates only the proxy, and restores the previous Compose image if live health checks fail.
5. Record the upstream tag and fork commit after deployment succeeds. Use a force-with-lease scoped to the fork's maintenance branch when publishing a rebased history.
6. Apply Debian security updates through the VM's signed APT sources. Leave kernel activation for a planned VM reboot.

Keep live configuration, SSH material, device keys, and OAuth files outside this repository.

## Public T3 Code hub limits

The public listener maps only T3 Code's account-list and quota-read paths to the
device-key-protected handlers. The responses contain anonymized account identifiers,
usage percentages and reset times. The gateway constructs provider requests itself.
Credential downloads, arbitrary API calls, balance and reset-credit details, and reset
operations are outside this interface. In T3 Code, use the public base URL and enter a
device key in the Management key field.

After deploying the proxy, pass `ops/hermes/nginx-public-hub.conf` on stdin to the
VM's `install-public-hub.sh` via SSH. This edits only the public listener, checks the
configuration before reload, and saves a copy of the previous edge configuration.
