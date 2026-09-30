# 0023. Jupiter runs its own 3-D Secure server against a simulated directory server

- Status: Accepted
- Date: 2026-09-30

## Context

3-D Secure 2 lets the issuer authenticate the cardholder before the authorization, either
at once (frictionless) or with a challenge, and moves liability for fraud to the issuer
when it does. The actors are the merchant's 3DS server, the scheme's directory server
(DS) and the issuer's access control server (ACS). They exchange AReq/ARes to decide,
CReq/CRes through the cardholder's browser for a challenge, and RReq/RRes to report its
result. The authorization then carries the authentication value (CAVV/AAV) and the
electronic commerce indicator (ECI). EMV 3DS 2.2 is the baseline since 2.1 was retired in
2024 ([research: card rails, 3DS2](../research/notes/04-card-rails.md)); the EMVCo
specification itself was not read, so most message details are unverified.

## Decision

**Jupiter is the 3DS server** (`internal/authentication`), behind a payments port like the
rail's. A customer-initiated live payment is authenticated when the merchant asks for it
(`request_three_d_secure: any`) or the risk engine returns `request_3ds`. The attempt is
`authenticating` until the answer, which drives the payment:

- Y (authenticated) or A (attempted): authorize, with the value and ECI, liability shifted;
- C (challenge): `requires_action`, with `next_action.redirect_to_url`;
- N or R: fail the attempt;
- U: authorize without authentication, liability staying with the merchant;
- no answer: stay `authenticating`. The resolver asks again, and fails the attempt
  (`authentication_unavailable`) after 15 minutes. Going on without authentication would
  let anyone who can make the directory server unreachable skip what the merchant or the
  risk engine asked for.

**The challenge.** Jupiter's page takes the customer's browser to the ACS with the CReq.
The ACS reports the result to Jupiter through the directory server (RReq), which
Jupiter answers (RRes) before resuming the payment on its own. The browser returns with
the CRes, and Jupiter sends it on to the merchant's `return_url` (HTTPS only, checked when
the merchant sends it). Only the RReq counts; the CRes only brings the customer back. The
browser is sent only to an ACS over HTTPS, and the page restricts where its form may post
(`Content-Security-Policy: form-action`) and cannot be framed.

**The simulator** (`sim-3ds`, [README](../../internal/sim/3ds/README.md)) plays the DS and
the ACS. Its authentication values are an HMAC under a key it shares with the card network
simulator, which checks the value on the 0100 and declines one that does not check out.

**Transport.** The AReq goes over HTTP to the directory server, which does not follow
redirects. The RReq is signed with an HMAC (`Threeds-Signature`), standing in for the
mutual TLS real directory servers use, and carries a final status: Y or A with an
authentication value, N or R.

The 3DS server reads the card without its security code, which stays in the vault for the
authorization.

The exit criteria are tests: the golden path authenticates frictionless before its ISO
8583 authorization, and `test/cardrail` completes a challenge through a scripted browser,
Jupiter's page, the ACS form and the one-time code, and back to the merchant.

## Alternatives rejected

- **A third-party 3DS server.** It would hide the part worth showing, the state machine
  around authentication, and cannot be simulated in CI.
- **Trusting the CRes the browser brings back.** The browser is the customer's, and can
  be replayed or altered; the RReq comes from the directory server.
- **Authenticating every payment.** Friction costs conversion; the merchant and the risk
  engine decide.

## Consequences

- A payment whose request stopped while authenticating is resumed by the resolver: the
  3DS server answers a repeat from its first answer.
- The 3DS server handles card numbers, in the AReq, and is in PCI DSS scope as a
  transmitter, like the rail. The link to the directory server carries them: in
  production it must be TLS (mutual, with the scheme); the simulator's is plain HTTP on
  the machine.
- 3DS Method (device fingerprinting), app-based flows and decoupled authentication are not
  simulated; a challenge is a one-time code.
