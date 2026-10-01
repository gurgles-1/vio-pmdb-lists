# vio-pmdb-lists

A native [Vio](https://github.com/drondeseries/Vio) plugin that reads your
[PublicMetaDB](https://publicmetadb.com) lists on a schedule and registers
every title as zero-storage virtual media in your Vio Movies and Series
virtual libraries.

Titles removed from a list are removed from Vio on the next clean sync.
Playback is handled by your configured virtual stream provider (e.g. the
Vio Virtual Library plugin with AIOStreams) — this plugin never touches
files and needs no storage.

## Install

Vio → Settings → Plugins → Add from URL, and use this catalog:

```
https://raw.githubusercontent.com/gurgles-1/vio-pmdb-lists/main/catalog.json
```

Then configure the plugin (Settings → Plugins → PMDB Lists → Settings):

- **PublicMetaDB API Key** — from your PublicMetaDB account settings.
- **PMDB List IDs** — one list ID per line (the ID in the list URL).
- **TMDB API Key** — v3 key or v4 read token; used for metadata, posters,
  and TMDB → IMDb resolution.
- **Movies Library / Series Library** — destination virtual libraries
  (Settings → Libraries → Add Virtual if you don't have them yet).
- **Sync Interval** — minimum minutes between automatic syncs (default 720).

Press **Sync now** on the plugin's admin page (PMDB Lists in the admin
nav) to run the first import immediately.

## How it works

1. The scheduled task fetches every configured PMDB list
   (`/api/external/lists/{id}/items`).
2. Each TMDB id is resolved through TMDB for metadata, posters, genres,
   runtime, the IMDb id (`external_ids`), and the full episode list for
   series.
3. Titles are registered with `UpsertVirtualMedia` under source key
   `pmdb-lists`, with canonical `virtual://movie/{imdb}` and
   `virtual://series/{imdb}/{season}/{episode}` URIs.
4. On a fully clean run, `ReconcileVirtualMedia` removes titles that left
   the lists. Any failed fetch disables reconciliation for that run so a
   bad API day can never wipe your library.

## Build

Requires Go 1.26+. The plugin uses the
[drondeseries/silo-plugin-sdk](https://github.com/drondeseries/silo-plugin-sdk)
virtual fork (`v0.13.2-virtual.2`), wired via a `replace` directive.

```bash
cd src
go build -o vio-pmdb-lists .
./vio-pmdb-lists manifest   # print the embedded manifest
```

## License

MIT
