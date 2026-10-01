# Bank simulator

`sim-bank` stands for the bank that holds Jupiter's account. It collects Jupiter's boletos,
makes its transfers and gives its statements. Jupiter reaches it over HTTP, as it would a
bank's file and API channels.

Its collection files are FEBRABAN's CNAB 240 v10.11, through
[`pkg/cnab240`](../../../pkg/cnab240). Its bank code, `999`, is no real bank's, and nothing
in it is connected to Núclea's boleto base.

```sh
JUPITER_SIM_BANK_CLIENTS='<token>:11222333000181:CONVENIO-0001:00001-0:000000123456-7' \
  go run ./cmd/sim-bank   # 127.0.0.1:8590 (JUPITER_HTTP_ADDR), admin on 127.0.0.1:8591
```

## Collection

| Behaviour | Source |
|---|---|
| A remittance is a CNAB 240 file of segments P and Q, with Y-03 for a hybrid boleto. The same file number again is answered as before, not applied twice | Sourced: CNAB 240 v10.11. Taking a file again is the simulator's |
| An entry is registered (occurrence 02) or rejected (03) with a reason: nosso número wrong or repeated, due date, amount, payer's CPF or CNPJ, payer's name | The occurrences are sourced. The rejection reasons are the layout's field codes as banks commonly publish them; the research did not check them against v10.11's table C047 |
| A hybrid boleto (distribution `P` or `Q`) is registered with a Pix location, returned in segment Y-03 with reason `P1` | Sourced: CNAB 240 v10.11 |
| A boleto is paid by its typed line or barcode (channel `33`, internet banking) or by its Pix code (channel `61`): occurrence 06 | Sourced: CNAB 240 v10.11 |
| A boleto unpaid past its due date plus the days the remittance allows is written off by the bank (09, reason `09`); one asked to be written off by file (movement 02) is written off (09, reason `10`) | Sourced codes. Writing off at the end of the term is the simulator's simplification of the bank's rule |
| What was paid is credited to the account the next business day | Common practice; the simulator's |
| Each day's close writes one return file of what happened since the last | The simulator's; banks differ in how often they return files |
| Not simulated: protests, discounts, interest and fines, partial payments, changes of amount or due date, Núclea's registration checks | |

## Transfers and statements

| Behaviour | Source |
|---|---|
| A transfer is named by the client: the same id again answers the same transfer; with other terms, it is refused | The simulator's |
| A transfer is made at the next close. One to account `999999` fails at once (reason `AC01`, account does not exist); one to `888888` is made and returned at the close after (`AC04`, account closed) | The reason codes are ISO 20022's, as Brazilian banks use them; the test accounts are the simulator's |
| A day's statement lists the account's credits and debits | The simulator's format |

## Operator controls

The admin address is unauthenticated plain HTTP; keep it on loopback.
- `POST /admin/boletos/pay {"line": …}` or `{"pix_code": …}` pays a boleto.
- `POST /admin/tick` closes the day now.
