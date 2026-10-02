package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/api/db"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

const (
	pointStarted   = "started"
	idempotencyHdr = "Idempotency-Key"
	maxBodyBytes   = 64 << 10
	maxKeyLength   = 255
	maxPoll        = 500 * time.Millisecond
	abandonedBatch = 100
	// maxCompleterRuns bounds how often the completer takes on one request, the runs
	// spaced further apart each time, over about a day: one that fails each time is
	// left, still unfinished, for a person (JupiterRequestsUnfinished).
	maxCompleterRuns = 30
	completerSource  = "completer"
	// runTimeout bounds a keyed request once it holds its key. The request runs on a
	// context detached from the client's, so a client that hangs up cannot abort it
	// half-way, and must finish well within the lock timeout.
	runTimeout     = 30 * time.Second
	releaseTimeout = 5 * time.Second
)

type idempotencyConfig struct {
	// lockTimeout is how long a runner may hold a key before the completer may take it
	// over. It is far longer than any request can run.
	lockTimeout time.Duration
	// wait is how long a request waits for a concurrent one holding the same key to
	// finish, so both return the same response, before giving up with 409.
	wait      time.Duration
	poll      time.Duration
	retention time.Duration
}

var defaultIdempotencyConfig = idempotencyConfig{
	lockTimeout: 5 * time.Minute,
	wait:        10 * time.Second,
	poll:        25 * time.Millisecond,
	retention:   24 * time.Hour,
}

type request struct {
	principal merchant.Principal
	version   string
	pathID    string
	body      []byte
	requestID string
	// state is what earlier phases handed on; it is saved with each recovery point.
	state map[string]string
	// scratch carries a foreign call's result to the atomic step of the same phase. It
	// is never saved: a recovery repeats the foreign call, which is idempotent.
	scratch any
}

// outcome ends an atomic phase: either the recovery point the next phase starts from,
// or the final response.
type outcome struct {
	next   string
	status int
	body   any
}

func proceed(next string) (outcome, error) {
	return outcome{next: next}, nil
}

func respond(body any) (outcome, error) {
	return outcome{status: http.StatusOK, body: body}, nil
}

// phase is one step of an operation. foreign, if set, calls something outside the
// database and must itself be idempotent, because a recovery repeats it. atomic runs in
// one transaction with the key's progress, so its effects and the recovery point
// commit together.
type phase struct {
	point   string
	foreign func(context.Context, *request) error
	atomic  func(context.Context, pgx.Tx, *request) (outcome, error)
}

type operation struct {
	name  string
	scope merchant.Scope
	// prepare, if set, runs before anything is recorded and may rewrite the request
	// body, which is then what the key stores and fingerprints. It keeps secrets such as
	// card numbers out of the database, and must give the same body for the same request.
	prepare func(ctx context.Context, req *request, idempotencyKey string) error
	phases  []phase
	// secret says the response carries a secret shown once: it is replayed only to the
	// key that made the request, not to any key of the account.
	secret bool
}

func (o operation) phase(point string) (phase, bool) {
	for _, p := range o.phases {
		if p.point == point {
			return p, true
		}
	}
	return phase{}, false
}

var (
	errLockLost = errors.New("idempotency key taken over by another runner")
	errBusy     = &Error{
		Status: http.StatusConflict, Type: "idempotency_error", Code: "idempotency_key_in_use",
		Message: "Another request with this Idempotency-Key is still being processed. Retry later.",
	}
	errMismatch = &Error{
		Status: http.StatusUnprocessableEntity, Type: "idempotency_error", Code: "idempotency_key_mismatch",
		Message: "This Idempotency-Key was used with a different request. Use a new key for a new request.",
	}
)

// serve runs a registered operation for an HTTP request, idempotently when the request
// carries an Idempotency-Key.
func (a *API) serve(w http.ResponseWriter, r *http.Request, opName, pathID string) {
	op := a.operations[opName]
	p := principalFrom(r.Context())
	if !p.Can(op.scope) {
		a.writeError(w, r, forbidden(string(op.scope)))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		a.writeError(w, r, invalidRequest("body_invalid", "", "The request body could not be read or is larger than 64 KB."))
		return
	}
	req := &request{
		principal: p, version: versionFrom(r.Context()), pathID: pathID, body: body,
		requestID: requestIDFrom(r.Context()), state: map[string]string{},
	}

	var status int
	var raw []byte
	key := r.Header.Get(idempotencyHdr)
	switch {
	case len(key) > maxKeyLength:
		err = invalidRequest("idempotency_key_invalid", idempotencyHdr, "Idempotency-Key is longer than %d characters.", maxKeyLength)
	case op.prepare != nil:
		err = op.prepare(r.Context(), req, key)
	}
	switch {
	case err != nil:
	case key == "":
		status, raw, err = a.runPhases(r.Context(), op, req, 0, pgtype.UUID{}, pointStarted)
	default:
		status, raw, err = a.runKeyed(r.Context(), op, req, key)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeRaw(w, status, raw)
}

func fingerprint(op string, pathID string, body []byte) ([]byte, error) {
	canonical := []byte("null")
	if len(bytes.TrimSpace(body)) > 0 {
		// UseNumber keeps large integers exact: as float64, two amounts differing above
		// 2^53 would fingerprint alike.
		var v any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, invalidRequest("body_invalid", "", "The request body is not valid JSON.")
		}
		// encoding/json writes object keys sorted, so equal JSON bodies fingerprint equally
		// whatever their key order or whitespace.
		var err error
		if canonical, err = json.Marshal(v); err != nil {
			return nil, err
		}
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(op), []byte(pathID), canonical} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte(":"))
		h.Write(part)
	}
	return h.Sum(nil), nil
}

type acquired struct {
	id     int64
	token  pgtype.UUID
	point  string
	state  []byte
	status int
	body   []byte
	busy   bool
}

func (a *API) runKeyed(ctx context.Context, op operation, req *request, key string) (int, []byte, error) {
	fp, err := fingerprint(op.name, req.pathID, req.body)
	if err != nil {
		return 0, nil, err
	}
	sealed, err := a.deps.Box.Seal(req.body, requestAD(req.principal, key))
	if err != nil {
		return 0, nil, err
	}
	deadline := time.Now().Add(a.idem.wait)
	poll := a.idem.poll
	for {
		got, err := a.acquire(ctx, op, req, key, fp, sealed)
		if err != nil {
			return 0, nil, err
		}
		switch {
		case got.body != nil:
			return got.status, got.body, nil
		case !got.busy:
			if err := restoreState(req, got.state); err != nil {
				return 0, nil, err
			}
			return a.runHeld(ctx, op, req, got.id, got.token, got.point)
		case time.Now().After(deadline):
			return 0, nil, errBusy
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(poll):
		}
		// A request that waits long waits for a long one: ask less often.
		poll = min(2*poll, maxPoll)
	}
}

// acquire records the key or finds it. It returns the stored response of a finished
// request, busy while another runner holds the key, or the lock and the recovery point
// to continue from.
func (a *API) acquire(ctx context.Context, op operation, req *request, key string, fp, sealed []byte) (acquired, error) {
	var got acquired
	now := a.deps.Now().UTC()
	err := postgres.InTx(ctx, a.deps.Pool, func(tx pgx.Tx) error {
		got = acquired{}
		q := db.New(tx)
		token := newLockToken()
		keyID, err := q.InsertIdempotencyKey(ctx, db.InsertIdempotencyKeyParams{
			MerchantID: req.principal.Merchant.String(), Livemode: req.principal.Livemode, Key: key,
			Fingerprint: fp, Operation: op.name, PathID: req.pathID, RequestBody: sealed,
			ApiVersion: req.version, KeyID: req.principal.Key.String(), KeyKind: string(req.principal.Kind),
			KeyScopes: scopeStrings(req.principal.Scopes), LockToken: token, Now: timestamptz(now),
		})
		if err == nil {
			got = acquired{id: keyID, token: token, point: pointStarted}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("recording idempotency key: %w", err)
		}
		row, err := q.LockIdempotencyKeyRow(ctx, db.LockIdempotencyKeyRowParams{
			MerchantID: req.principal.Merchant.String(), Livemode: req.principal.Livemode, Key: key,
		})
		if err != nil {
			return fmt.Errorf("locking idempotency key: %w", err)
		}
		switch {
		case !bytes.Equal(row.Fingerprint, fp), op.secret && row.KeyID != req.principal.Key.String():
			return errMismatch
		case row.ResponseStatus.Valid:
			body, err := a.deps.Box.Open(row.ResponseBody, responseAD(row.ID))
			if err != nil {
				return err
			}
			got = acquired{status: int(row.ResponseStatus.Int32), body: body}
			return nil
		case row.LockToken.Valid && row.LockedAt.Time.After(now.Add(-a.idem.lockTimeout)):
			got = acquired{busy: true}
			return nil
		}
		if err := q.TakeIdempotencyLock(ctx, db.TakeIdempotencyLockParams{ID: row.ID, LockToken: token, Now: timestamptz(now)}); err != nil {
			return err
		}
		got = acquired{id: row.ID, token: token, point: row.RecoveryPoint, state: row.RecoveryState}
		return nil
	})
	return got, err
}

func restoreState(req *request, raw []byte) error {
	req.state = map[string]string{}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &req.state); err != nil {
		return fmt.Errorf("reading recovery state: %w", err)
	}
	return nil
}

// runHeld runs the phases of a request that holds its key, detached from the client's
// context, and releases the key if a phase panics.
func (a *API) runHeld(ctx context.Context, op operation, req *request, keyID int64, token pgtype.UUID, point string) (status int, raw []byte, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()
	defer func() {
		if v := recover(); v != nil {
			if err := a.release(ctx, keyID, token); err != nil {
				a.deps.Logger.ErrorContext(ctx, "releasing idempotency key after a panic", "key", keyID, "error", err)
			}
			panic(v)
		}
	}()
	return a.runPhases(ctx, op, req, keyID, token, point)
}

func (a *API) release(ctx context.Context, keyID int64, token pgtype.UUID) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	return db.New(a.deps.Pool).ReleaseIdempotencyKey(ctx, db.ReleaseIdempotencyKeyParams{ID: keyID, LockToken: token})
}

// runPhases runs op from point to its end. keyID is 0 for a request without an
// Idempotency-Key, which then records nothing.
func (a *API) runPhases(ctx context.Context, op operation, req *request, keyID int64, token pgtype.UUID, point string) (int, []byte, error) {
	for {
		ph, ok := op.phase(point)
		if !ok {
			return 0, nil, fmt.Errorf("operation %s has no phase %q", op.name, point)
		}
		if ph.foreign != nil {
			if err := ph.foreign(ctx, req); err != nil {
				return a.settleFailure(ctx, req, keyID, token, err)
			}
		}
		next, status, raw, err := a.runAtomic(ctx, ph, req, keyID, token)
		switch {
		case errors.Is(err, errLockLost):
			return 0, nil, errBusy
		case err != nil:
			return a.settleFailure(ctx, req, keyID, token, err)
		case next == "":
			return status, raw, nil
		}
		if a.afterPhase != nil {
			a.afterPhase(next)
		}
		point = next
	}
}

// runAtomic runs one phase's transaction. It returns the next recovery point, or the
// final status and rendered body, which it stores with the key before committing.
func (a *API) runAtomic(ctx context.Context, ph phase, req *request, keyID int64, token pgtype.UUID) (next string, status int, raw []byte, err error) {
	err = postgres.InTx(ctx, a.deps.Pool, func(tx pgx.Tx) error {
		next, status, raw = "", 0, nil
		q := db.New(tx)
		if keyID != 0 {
			if _, lockErr := q.CheckIdempotencyLock(ctx, db.CheckIdempotencyLockParams{ID: keyID, LockToken: token}); errors.Is(lockErr, pgx.ErrNoRows) {
				return errLockLost
			} else if lockErr != nil {
				return lockErr
			}
		}
		out, phaseErr := ph.atomic(ctx, tx, req)
		if phaseErr != nil {
			return phaseErr
		}
		if out.next != "" {
			next = out.next
			if keyID == 0 {
				return nil
			}
			state, stateErr := json.Marshal(req.state)
			if stateErr != nil {
				return stateErr
			}
			return q.AdvanceIdempotencyKey(ctx, db.AdvanceIdempotencyKeyParams{
				RecoveryPoint: next, RecoveryState: state, ID: keyID, LockToken: token,
			})
		}
		rendered, renderErr := render(out.body, req.version)
		if renderErr != nil {
			return renderErr
		}
		status, raw = out.status, rendered
		return a.finish(ctx, q, keyID, token, status, raw)
	})
	return next, status, raw, err
}

func (a *API) finish(ctx context.Context, q *db.Queries, keyID int64, token pgtype.UUID, status int, raw []byte) error {
	if keyID == 0 {
		return nil
	}
	sealed, err := a.deps.Box.Seal(raw, responseAD(keyID))
	if err != nil {
		return err
	}
	n, err := q.FinishIdempotencyKey(ctx, db.FinishIdempotencyKeyParams{
		ResponseStatus: pgtype.Int4{Int32: int32(status), Valid: true}, //nolint:gosec // an HTTP status code
		ResponseBody:   sealed, ID: keyID, LockToken: token,
	})
	if err == nil && n == 0 {
		return errLockLost
	}
	return err
}

// settleFailure stores a client error as the key's final response, since repeating the
// request would fail the same way. Any other failure releases the key without a
// response, so a retry, or the completer, runs the operation on from its recovery point.
func (a *API) settleFailure(ctx context.Context, req *request, keyID int64, token pgtype.UUID, cause error) (int, []byte, error) {
	apiErr, isClientError := asError(cause)
	if keyID == 0 {
		return 0, nil, cause
	}
	if !isClientError || apiErr.Status >= http.StatusInternalServerError || apiErr == errMismatch || apiErr == errBusy {
		return 0, nil, errors.Join(cause, a.release(ctx, keyID, token))
	}
	raw, err := render(apiErr.body(req.requestID), CurrentVersion)
	if err != nil {
		return 0, nil, err
	}
	err = postgres.InTx(ctx, a.deps.Pool, func(tx pgx.Tx) error {
		return a.finish(ctx, db.New(tx), keyID, token, apiErr.Status, raw)
	})
	if err != nil {
		return 0, nil, err
	}
	return apiErr.Status, raw, nil
}

// CompleteAbandoned finishes requests that committed some work and then stopped, and
// returns how many it completed. Requests that stopped before committing anything are
// only unlocked, so the client's retry runs them and the reaper can remove them.
func (a *API) CompleteAbandoned(ctx context.Context) (int, error) {
	staleBefore := a.deps.Now().UTC().Add(-a.idem.lockTimeout)
	q := db.New(a.deps.Pool)
	if err := q.ReleaseStaleStartedKeys(ctx, timestamptz(staleBefore)); err != nil {
		return 0, fmt.Errorf("unlocking requests that did nothing: %w", err)
	}
	ids, err := q.AbandonedIdempotencyKeys(ctx, db.AbandonedIdempotencyKeysParams{
		StaleBefore: timestamptz(staleBefore), MaxRuns: maxCompleterRuns, MaxCount: abandonedBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("finding abandoned requests: %w", err)
	}
	completed := 0
	var failures []error
	for _, keyID := range ids {
		done, err := a.complete(ctx, keyID, staleBefore)
		if err != nil {
			failures = append(failures, fmt.Errorf("completing idempotency key %d: %w", keyID, err))
		}
		if done {
			completed++
		}
	}
	return completed, errors.Join(failures...)
}

func (a *API) complete(ctx context.Context, keyID int64, staleBefore time.Time) (bool, error) {
	var row db.ApiIdempotencyKey
	token := newLockToken()
	taken := false
	err := postgres.InTx(ctx, a.deps.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		if row, err = q.LockIdempotencyKeyByID(ctx, keyID); err != nil {
			return err
		}
		stale := !row.LockToken.Valid || row.LockedAt.Time.Before(staleBefore)
		if row.ResponseStatus.Valid || !stale {
			return nil
		}
		taken = true
		if err := q.CountCompleterRun(ctx, keyID); err != nil {
			return err
		}
		return q.TakeIdempotencyLock(ctx, db.TakeIdempotencyLockParams{ID: keyID, LockToken: token, Now: timestamptz(a.deps.Now().UTC())})
	})
	if err != nil || !taken {
		return false, err
	}
	op, ok := a.operations[row.Operation]
	if !ok {
		return false, fmt.Errorf("unknown operation %q", row.Operation)
	}
	req, err := a.storedRequest(row)
	if err != nil {
		return false, err
	}
	if _, _, err := a.runHeld(ctx, op, req, keyID, token, row.RecoveryPoint); err != nil {
		if _, isClientError := asError(err); !isClientError {
			return false, err
		}
	}
	return true, nil
}

func (a *API) storedRequest(row db.ApiIdempotencyKey) (*request, error) {
	merchantID, err := merchant.MerchantPrefix.Parse(row.MerchantID)
	if err != nil {
		return nil, err
	}
	keyID, err := merchant.KeyPrefix.Parse(row.KeyID)
	if err != nil {
		return nil, err
	}
	scopes := make([]merchant.Scope, len(row.KeyScopes))
	for i, s := range row.KeyScopes {
		scopes[i] = merchant.Scope(s)
	}
	principal := merchant.Principal{
		Merchant: merchantID, Key: keyID, Livemode: row.Livemode, Kind: merchant.Kind(row.KeyKind),
		Scopes: scopes, APIVersion: row.ApiVersion,
	}
	body := row.RequestBody
	if row.RequestSealed {
		if body, err = a.deps.Box.Open(row.RequestBody, requestAD(principal, row.Key)); err != nil {
			return nil, fmt.Errorf("opening the stored request: %w", err)
		}
	}
	req := &request{
		principal: principal,
		version:   row.ApiVersion, pathID: row.PathID, body: body,
		requestID: requestPrefix.New().String() + "_" + completerSource,
	}
	return req, restoreState(req, row.RecoveryState)
}

// ReapIdempotencyKeys deletes keys past their retention and returns how many.
// Unfinished keys that committed work are kept for the completer.
func (a *API) ReapIdempotencyKeys(ctx context.Context) (int64, error) {
	n, err := db.New(a.deps.Pool).ReapIdempotencyKeys(ctx, timestamptz(a.deps.Now().UTC().Add(-a.idem.retention)))
	if err != nil {
		return 0, fmt.Errorf("reaping idempotency keys: %w", err)
	}
	return n, nil
}

func newLockToken() pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

func responseAD(keyID int64) []byte {
	return []byte("idempotency_key/" + strconv.FormatInt(keyID, 10))
}

// requestAD binds a stored request body to its account, mode and key.
func requestAD(p merchant.Principal, key string) []byte {
	return []byte("idempotency_request/" + p.Merchant.String() + "/" + strconv.FormatBool(p.Livemode) + "/" + key)
}

func scopeStrings(scopes []merchant.Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}
