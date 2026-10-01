# Settlement simulator

`sim-slc` stands for centralized settlement (the SLC, run by Núclea). Since Res. BCB
522/2025, every sub-acquirer takes part in it, as a payer to its merchants
([research §1](../../../docs/research/notes/02-brazil-regulation-and-receivables.md)).
Jupiter reaches it over HTTP.

Núclea's layouts and its SLC message catalogue were not available to the research. The wire
format ([`pkg/slcapi`](../../../pkg/slcapi/slcapi.go)) is the simulator's own.

```sh
JUPITER_SIM_SLC_PARTICIPANTS='<token>:11222333000181:30000001' \
  go run ./cmd/sim-slc   # 127.0.0.1:8592 (JUPITER_HTTP_ADDR), admin on 127.0.0.1:8593
```

| Behaviour | Source |
|---|---|
| A participant submits each business day a grade: each payment of each unit due by that day, to its beneficiary at its domicile | Sourced in principle: Res. BCB 522/2025, Núclea. The grade's shape is the simulator's |
| The same grade again is answered as before; another for the same day is refused | The simulator's |
| At the settlement window, entries at the participant's own ISPB are credited to its settlement account, and the rest is paid to the other institutions directly | Sourced in principle: a payment's domicile is where the registry says it settles |
| Anticipations are reported per unit; one reported after the business day following it is listed as late | Sourced: the "informativo via antecipação", same day or D+1 (NDM Advogados, via the research) |
| Not simulated: the network's side (what each acquirer pays in), the windows' times, rejections of single entries, netting between participants, JWS signatures and the RSFN | |

## Operator controls

The admin address is unauthenticated plain HTTP; keep it on loopback.
- `POST /admin/tick` runs the settlement window now.
