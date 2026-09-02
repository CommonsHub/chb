# Data model

The on-disk shape `chb` reads and writes. Schemas first, file layout after.

## TransactionEntry

The canonical transaction shape, defined in `cmd/generate.go`. One per money-movement event from one account's perspective — see [transactions.md](transactions.md) for the full schema (signed-amount convention, account-based vs graph-based design, why there's no `from`/`to`, per-provider quirks).

Short version:

- **Account-based.** Each entry has an `accountId` and the amount is signed from that perspective. Internal transfers produce two entries (one per account), sharing the same `id`.
- **No `from` / `to` / `sender` / `receiver` fields.** Use `accountId` (the perspective) and `counterpartyId` (the other side).
- **Signed `amount`:** positive ⇔ money INTO `accountId`. `grossAmount` is always positive; `netAmount` is signed after fees.
- **Not in JSON, but on the in-memory struct:** `AccountCode`, `PartnerID` (Odoo-specific; live in `providers/odoo/pending/`).

## FullEvent

Defined in `cmd/events_generate.go`. One per public event or room booking.

Key fields: `id`, `name`, `start` / `end` (RFC3339 with Brussels offset, never naïve), `allDay`, `room`, `visibility`, `host`, `attendees`, `ticketUrl`, `metadata`.

All-day events (`allDay=true`) must be rendered without a clock time — see [philosophy.md](philosophy.md).

## Message

Defined in `cmd/messages_sync.go`. One per Discord message in a tracked channel: `channelId`, `messageId`, `author`, `content`, `timestamp`, `attachments`.

## File layout

The on-disk root is `$DATA_DIR` (default `$APP_DATA_DIR/data` → `~/.chb/data`). Three top-level shapes:

```
$DATA_DIR/
├── YYYY/MM/
│   ├── providers/<provider>/...        # raw provider archives (one per source)
│   ├── processors/<processor>/...      # cross-provider enrichment outputs
│   └── generated/                       # public outputs derived from providers/
│       ├── transactions.json
│       ├── events.json
│       ├── messages.json
│       ├── images.json
│       ├── counterparties.json
│       └── private/                     # PII layer, requires --with-pii
│           └── enrichment.json
└── latest/
    └── generated/                       # the most recent month's outputs, mirrored
```

### `providers/<provider>/`

Raw provider state, archived unchanged. Re-running `sync` against the same period must produce identical files (idempotency invariant).

Examples:
- `providers/stripe/transactions.json` — Stripe balance-transactions for the month.
- `providers/etherscan/gnosis/<slug>.<symbol>.json` — Etherscan-format transfers per (chain, account, token).
- `providers/ics/<slug>.ics` — raw ICS feed bytes.
- `providers/odoo/<entity>.json` — Odoo journal/move/partner caches.

### `providers/<target>/pending/`

Targets (Odoo, Nostr) have an extra `pending/` folder that holds the changes the next `push` would publish:

```
providers/odoo/pending/transactions.json
```

Schema: `{ generatedAt, entries: { <txUri>: { accountCode, partnerId, category, collective } } }`. Written by `generate`, read by every push path. Inspect with `git diff providers/odoo/pending/2026-05/transactions.json`.

Nostr's equivalent lives at `$APP_DATA_DIR/nostr/outbox/` for historical reasons (the outbox holds signed-but-unsent events). List with `chb nostr pending`. Both serve the same role — pending changes you can inspect before publishing.

### `generated/`

Every file `chb generate` produces lives here. Vendor-agnostic — no Odoo IDs, no partner-IDs, no Stripe-internal handles beyond what's needed to round-trip. Push paths *load* from here, then enrich with the target-specific `pending/` entries.

### `latest/`

A mirror of the most recent month's `generated/` files, plus aggregated multi-month files (e.g. `latest/generated/events.json` covers everything upcoming, not just one month). Convenient for downstream consumers that want "current state" without computing month bounds.

### `generated/private/` and `generated/restricted/`

Two non-public trees, distinguished by whether a reader exists at all:

- **`private/`** is served to nobody. Operator-only material — PII enrichment, anything a request should never reach. Downstream consumers must not expose it under any condition.
- **`restricted/`** is served, but only to the person it describes, and only once they have proved who they are.

Both path segments are load-bearing in the same way: `writeDataFile` gives either tree 0700 directories, and the PII guard skips both rather than scrubbing names and emails the way it does for public files. They differ only in who may read them, which is a decision for whatever serves the data.

`latest/generated/restricted/members/<emailHash>.json` holds one member's month-by-month standing from 2026-01 onwards — see [Membership identity](#membership-identity).

## Membership identity

A member's id is `emailHash`: `sha256(lowercase(trim(email)) + EMAIL_HASH_SALT)`. It is the join key across months, the filename of their history, and the only way the website can recognise a signed-in person as a member.

**The salt is the identity.** The same email must always produce the same digest, on every machine that generates member data and on the website host that resolves it. Rotating the salt re-identifies the entire membership: every id changes, every history splits in two, and nothing links the halves. This has gone wrong twice — once when `chb setup` dropped the key from `config.env` on rewrite (see `cmd/setup.go`), and once when 2026-04 was written under a different salt and its 61 continuing members read as 61 one-month strangers.

Consequences of that, all deliberate:

- `chb members sync` refuses to run without `EMAIL_HASH_SALT` rather than minting one. First-ever setup uses `--init-salt`, which mints and persists a salt exactly once.
- `chb generate` warns when a month shares no membership id with the month before it — the signature of a salt change.
- The website host needs the same `EMAIL_HASH_SALT` to hash a signed-in user's email and find their record. Without it, it cannot identify anyone and shows no member data at all. That is the intended failure mode: no salt, no membership surface.
- `config.env` is excluded from mirror sync (see [mirror-mode.md](mirror-mode.md)), so the salt is copied between hosts deliberately, by a human, and never by rsync.

`chb members whois <email>` hashes an address with the configured salt and reports the membership on file for it — the tool for "why can't this person see their membership" (usually: they subscribed under a different address than the one on their Discord account). It also accepts a membership id directly, which needs no salt.

### Linking a member's identifiers

A member pays with one address and signs in with another often enough that the two look like different people, or like nobody. `settings/member-links.json` says which identifiers belong to the same person:

```json
{
  "links": [
    {
      "identifiers": [
        "discord:user:123456789012345678",
        "email:sha256:<hash of their Stripe address>",
        "email:sha256:<hash of an older address>"
      ],
      "note": "pays with a personal address, Discord uses the work one"
    }
  ]
}
```

Identifiers follow the same [NIP-73 URI convention](#uris-nip-73) as everything else — `discord:user:<snowflake>`, `nostr:pubkey:<hex>`, `email:sha256:<emailHash>`. **No kind is privileged.** Discord is what most people sign in with today; that is a fact about the deployment, not about the code, and adding Nostr auth means adding entries of another kind rather than another special case. Note that a Discord *username* is not an identifier: usernames have been mutable since 2023, so links key on the account id.

The canonical member id — which names the history file — is the entry's explicit `id` when set, otherwise the first `email:sha256:` hash. That leaves every unlinked member's filename exactly as it was, so introducing the file migrates nothing. `id` exists for a member with no email in the system at all, whose handle should not be derived from any provider; without either, a digest of the first identifier gives them a stable id in the same shape.

A linked member's history is the **union of months across all their aliases**, so someone who changed address gets one continuous timeline rather than two fragments. `latest/generated/restricted/members-index.json` maps every identifier to its member, so a caller holding a Discord id — or tomorrow a pubkey — finds the history without knowing any email. It sits in `restricted/` because *that a given Discord account belongs to a member* is exactly the fact we do not publish.

### Funders

A third membership source, after Stripe and Odoo: memberships paid outside both — a bank transfer, a grant, a membership someone gifted. Nothing fetches them, so `settings/funders.json` states each term outright and chb works out which months it covers:

```json
{
  "funders": [
    {
      "emailHash": "<64-hex, from `chb members whois <email>`>",
      "firstName": "Ada",
      "startsAt": "2026-01-01",
      "expiresAt": "2026-12-31",
      "amount": { "value": 120, "decimals": 2, "currency": "EUR" },
      "interval": "year",
      "note": "Bank transfer, invoice CHB/2026/00123"
    }
  ]
}
```

- `emailHash` is preferred over `email`; both work, but the hash keeps addresses out of a file that syncs between hosts. An `email` is hashed at sync time and so needs `EMAIL_HASH_SALT`.
- `expiresAt` is required. A funder without an end date is an open-ended claim nobody reviews.
- The membership is paid **until** `expiresAt`, so the month containing that date is covered in full — someone paid to the 15th is a member for that month, not two thirds of one.
- Stripe and Odoo are the systems of record: a person who appears in either keeps that entry rather than being relabelled a funder.
- A malformed entry is reported and skipped; one bad date does not cost the month's remaining membership.

The file is edited by hand and is not shipped as a default, so nothing overwrites it.

## URIs (NIP-73)

Every entity has a URI used as its canonical handle:

| Entity | URI form |
|---|---|
| Blockchain tx | `ethereum:<chainId>:tx:<hash>` |
| Blockchain address | `ethereum:<chainId>:address:<addr>` |
| Token contract | `ethereum:<chainId>:token:<contract>` |
| Stripe object | `stripe:<id>` (the id carries its own type prefix `txn_…`, `cus_…`, `ch_…`) |
| Bank tx | `iban:<iban-lowercase>:tx:<row-hash>` |
| Bank account | `iban:<iban-lowercase>` |

These are also the `i` tags used by Nostr kind-1111 annotations, so the same key works on both sides.

## Settings vs data

Settings live in `$APP_DATA_DIR/settings/` (see [README.md](../README.md#settings)). Data lives in `$DATA_DIR`. The two are deliberately separate roots so settings can be checked into a private git repo while data stays out.

`$APP_DATA_DIR/nostr/` holds the Nostr outbox + sent events (Nostr's pending state) and signing keys.
