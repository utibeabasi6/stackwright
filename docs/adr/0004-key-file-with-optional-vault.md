# 0004 — A key file by default, Vault Transit optional

**Status:** Accepted, 2026-11-11.

## Context

State, sensitive variables and stored secrets are sealed with a data key per
org. Something must wrap those data keys. The first build used Vault Transit
(its ADR 0030). Vault is one more service to run, unseal and back up.

## Decision

A key-encryption key (KEK) wraps each org's data key. The KEK comes from one
of two sources:

1. **A key file (default).** `stackwright init` writes 32 random bytes,
   base64, to `.env` as `STACKWRIGHT_KEK`. `STACKWRIGHT_KEK_FILE` reads it
   from a mounted file instead.
2. **Vault Transit (optional).** When `STACKWRIGHT_VAULT_ADDR` is set, Vault
   wraps and unwraps data keys. The KEK never leaves Vault.

Both sit behind one Go interface with `Wrap` and `Unwrap`. Every wrapped key
records which KEK sealed it, so a rotation can rewrap old keys.

## Alternatives

- **Vault required.** It is stronger, but every small install must run Vault.
- **A cloud KMS.** It ties a self-hosted install to one cloud. It can be a
  third implementation later.

## Consequences

- Losing `.env` loses every state file. The setup page and the docs say so,
  and the backup guide in phase 22 covers it.
- `stackwright rotate-kek` rewraps every data key under a new KEK.
- The server refuses to start without a KEK. It has no default key.
