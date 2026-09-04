# Providers

Providers own upstream data. A provider is responsible for downloading provider
data, splitting it into the monthly `DATA_DIR/YYYY/MM/providers/<provider>/`
archive layout, and reading that archived data back without calling the provider
again for past months.

Each provider should have a directory under `providers/<provider>/` with:

- `source.go`: provider identity, declared output files, and provider storage
  helpers.
- One file per provider data family where practical, for example
  `transactions.go`, `charges.go`, `customers.go`, `subscriptions.go`,
  `payouts.go`, or `balance.go`.
- `types.go` only for shared provider data structures used by multiple files.

The shared provider descriptor lives in `providers/source.go`:

```go
type Provider interface {
	Name() string
	Files() []File
}
```

`cmd/` should orchestrate commands and wire provider data into existing CLI flows.
Provider API calls, provider file paths, provider archive readers, and provider
object types should live in the provider package.

Current providers:

- `providers/stripe`: Stripe balance transactions, charges, customers,
  subscriptions, and payouts.
- `providers/etherscan`: Etherscan V2 ERC20 transfer archives. Chain-specific
  data lives under `providers/etherscan/<chain>/`.
- `providers/monerium`: Monerium SEPA order archives.
- `providers/nostr`: Nostr annotations and chain tx/address metadata.
- `providers/discord`: Discord messages for monitored channels, forum threads
  for monitored forum channels (`<forumId>/threads/<threadId>.json` plus a
  `<forumId>/forum.json` snapshot — see `chb proposals sync`), and downloaded
  Discord image attachments referenced by `generated/images.json`. A forum the
  bot may list but not read is archived with `"metadataOnly": true` (titles,
  ids and activity dates only); a later sync with read access replaces those
  files, and a full archive is never downgraded back to metadata.
- `providers/odoo`: Odoo invoices, bills, subscriptions, analytic enrichment,
  and private attachment metadata/binaries.
- `providers/ics`: Monthly ICS calendar archives for room bookings and configured
  calendars.

Monthly generation writes `generated/summary.json` after the other generated
files. Each provider can contribute provider-specific record, attachment, and
summary counts through the monthly report contributor hook in `cmd`. A
cross-month rollup pass then fills in per-collective `startBalance` /
`endBalance` and writes the global aggregate to
`latest/generated/summary.json`.

Derived public calendar exports and event cover image downloads live under
`generated/` (for example `generated/calendars/public.ics` and
`generated/events/images/`) because they are generated/enriched artifacts, not
provider archives.
