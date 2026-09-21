# cline-cpa-plugin

Cline / ClinePass OAuth provider plugin for
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA).

The plugin is OAuth-only by design. Credentials come from Cline's WorkOS
device flow, so an API key is not required.

## Features

- WorkOS device login, returning a CPA auth record
- Cline OAuth refresh (`/api/v1/auth/refresh`)
- Cline recommended model catalog (`recommended`, `free`, `clinePass`, `clineCloud`)
- Plugin-owned model hide/order/add configuration
- Streaming and non-streaming OpenAI-compatible execution
- Management panel with model controls and account plan status
- Entitlement errors are reported clearly and never put the whole auth into
  cooldown

## Build

```bash
make build
```

Output: `cline.so`.

## Install

Copy `cline.so` to the CPA plugins directory (for example
`/opt/cpa/plugins/cline.so`) and reload CPA.

## Notes

ClinePass models require an active ClinePass subscription on the Cline account.
Free models exposed under the ClinePass provider are also registered, but are
marked as free in the model name.
