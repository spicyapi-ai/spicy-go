# Changelog

## v0.3.0 — 2026-10-23

### Added

- **The asset library.** Saved files are kept until they are deleted and are referenced as
  `spicy://a/<id>` in any media field of model inputs sent with keys of the same workspace; an
  upload's `spicy://` URI lasts one day. The key decides the library: a personal key reaches the
  personal library, a team key that team's shared library, where the creator's current role decides
  what may change.
  - `SaveAsset(ctx, SaveAssetInput, idempotencyKey)` saves a task output (`TaskID` with `Key` or
    `Index`), a committed upload (`FileID`) or a public URL (`URL`). It sends the required
    `Idempotency-Key` itself when the argument is empty, the same one on every retry of the call;
    pass your own to replay a save.
  - `ListAssets` and `EachAsset` take `ListAssetsOptions`: `Kind`, `Source` (one or several), `Q`,
    `Favorite` (a `*bool`, so `false` lists the assets that are not favorites), `CollectionID`,
    `EntityID`, `Sort` (`AssetSortRecent`, `AssetSortOldest`, `AssetSortName`, `AssetSortSize`),
    `Cursor` and `Limit`. `EachAsset` follows `NextCursor` and repeats the filters and sort on every
    page; a cursor that comes back twice is refused instead of looping.
  - `GetAsset`, `UpdateAsset` (`Name`, `Favorite` or both; only what you set is sent),
    `DeleteAsset` and `GetAssetUsage`, which carries `Scope`.
  - `UploadAssetFile(ctx, path, UploadAssetOptions)` uploads a local file and saves it in one call;
    the name defaults to the file name, cut to 80 characters.
  - `ListCharacters`, `EachCharacter` and `GetCharacter` read the library's characters (named,
    ordered sets of images) with their images in order; `AssetCharacter.URIs` lists their
    references. Characters are read-only through the API.
  - Asset IDs are accepted as `ast_…` or as the `spicy://a/…` reference. Empty IDs, blank filters,
    an unknown `Sort` and a save that mixes two sources fail before anything is sent.
  - New types: `LibraryAsset` (a saved file; `Asset` remains one output of a task), `AssetList`,
    `AssetCharacter`, `AssetCharacterList`, `DeletedAsset`, `AssetUsage`, `AssetPlan`,
    `AssetScope`, `AssetLimits` and `AssetUsageTotals`.
- **`APIError.Reason`**, read from a failure envelope's `data.reason`. The asset library uses
  `asset_library_locked`, `asset_library_full`, `asset_file_too_large`, `asset_daily_limit`,
  `asset_storage_full`, `asset_permission_denied`, `asset_workspace_forbidden` and
  `asset_entity_name_taken`, exported as the `ReasonAsset*` constants with `AssetErrorReasons` and
  `IsAssetErrorReason`; any other value is passed through unchanged. `APIError.Error` names the
  reason when there is one.
- **LoRA weight uploads.** `UploadFile` maps `.safetensors` to `application/octet-stream`, the
  upload type for `loras[].path`, with its own 2 GiB ceiling.

### Changed

- `UploadFile` streams the file from disk with a declared length instead of reading it into memory
  first, so a 2 GiB LoRA file does not need 2 GiB of memory. It also refuses an empty file and
  anything that is not a regular file before asking for a ticket.
- A retryable failure whose `Retry-After` is longer than this client waits between attempts (8
  seconds), or than the context has left, is now returned at once with its `RetryAfter` and
  `Reason`. It used to be retried early against a limit that had not moved, or end in the
  context's own deadline error, which hid what the server said; the asset library's daily limit
  (429 until 00:00 UTC) is the case that made this visible. Shorter waits are retried as before.
- The pinned `contracts/openapi.yaml` matches the contract published on 2026-10-23, including the
  asset routes and `POST /api/v1/files/import` (which this package does not wrap).
  `TestContractHasNotDrifted` also pins the asset reasons, sorts and sources.

## v0.2.0 — 2026-10-05

### Module path

The repository moved to the `spicyapi-ai` GitHub organization, and the module path moved with it:

```
github.com/Spicy-API/spicy-go   ->   github.com/spicyapi-ai/spicy-go
```

v0.1.x stays available under the old path, but every release from this one on exists only under
the new one. To upgrade, change the import path and fetch the new module:

```bash
go get github.com/spicyapi-ai/spicy-go@v0.2.0
```

then replace `github.com/Spicy-API/spicy-go` with `github.com/spicyapi-ai/spicy-go` in your imports
and run `go mod tidy`. The package name is still `spicy`, so no other line changes.

### Added

- Reference documents can be uploaded: PDF, Word, Excel, PowerPoint, Keynote, Pages, Numbers,
  plain text and Markdown. Before this release the client refused them locally. `UploadFile`
  infers the type from the extension for all of them except Keynote, because `.key` is also the
  usual name of a PEM private key; pass `application/vnd.apple.keynote` explicitly.
- `TaskOutput.Transcript` (`TaskTranscript`, `TaskTranscriptWord`, `TaskTranscriptChannel`): the
  structured speech-to-text result, with word timings, speaker labels, the detected language and
  per-channel transcripts.
- `Asset.Layer` (`AssetLayer`): stacking order, name and bounding box for the outputs of a
  layer-decomposition task.
- `TaskListItem.RequestID`: the `X-Request-Id` of the request that created a task, which ties a
  synchronous text call made through the compatible endpoints to its charge.
- `Model.Toolkit`: the catalogue grouping, now `subject-swap` or `studio-tools`.
- `CodeEmailNotVerified` (40310): the account's email address is not verified yet, so its keys
  cannot run models.

### Changed

- The pinned contract in `contracts/openapi.yaml` is refreshed to the one published on 2026-10-05.
- Documentation now covers audio models, the `per_1k_characters` price unit, persona images as
  `spicy://p/<assetId>` in image fields, the `edit` task filter (which also selects the `image-edit`
  tools), the one case where a failed task is charged (a `content_rejected` refusal on a model whose
  page says refusals are billed), and using uploaded `spicy://f/` URIs in chat message parts.

## v0.1.2 — 2026-09-20

Identical to v0.1.1 apart from the `retract v0.1.0` directive in `go.mod`.

## v0.1.1 — 2026-09-20

v0.1.0 with every source comment in English.

## v0.1.0 — 2026-09-20 (retracted)

Initial release: the asynchronous generation flow, the three-step upload and webhook verification.
