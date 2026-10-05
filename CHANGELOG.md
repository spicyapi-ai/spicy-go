# Changelog

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
