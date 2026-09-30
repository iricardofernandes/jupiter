# API Pix specification

`openapi.yaml` is the Banco Central's API Pix specification, unchanged:

| | |
|---|---|
| Version | 2.10.0 (matches the Manual de Padrões para Iniciação do Pix v2.10.0, 19/08/2026) |
| Source | https://github.com/bacen/pix-api, `openapi.yaml` at commit `55b60d7948a5436643c8571a82d59eabed2d61e9` |
| SHA-256 | `5c39d711674551c2ce2ced9263724d58237ea84cf9594950a5c479a96e354310` |
| License | Apache 2.0 |

`make generate` turns it into the Go types in `pkg/pixapi`, for the resources Jupiter
uses (tags Cob, CobV, Pix, Webhook, PayloadLocation, CobPayload, and for Pix Automático
Rec, RecPayload, SolicRec, CobR, PayloadLocationRec, WebhookRec and WebhookCobR), through
`overlay.yaml`, which works around what oapi-codegen cannot express: it drops the fields
of a `oneOf` inside an `allOf`. The overlay spells those objects out as plain objects:
the payer (`devedor`), the receiver (`recebedor`), a discount's dates, a recurrence's or
a recurring charge's end (`encerramento`), and a recurrence request's addressee
(`destinatario`). The specification file itself
is never edited; to move to a new version, replace it, record the new commit and hash
here, and run `make generate`.
