-- name: InsertTransaction :one
INSERT INTO authentication.transactions (server_trans_id, attempt_id, intent_id, merchant_id, livemode, trans_status,
                                         ds_trans_id, acs_trans_id, acs_url, eci, authentication_value, return_url,
                                         created_at, updated_at)
VALUES (@server_trans_id, @attempt_id, @intent_id, @merchant_id, @livemode, @trans_status, @ds_trans_id, @acs_trans_id,
        @acs_url, @eci, @authentication_value, @return_url, @now, @now)
ON CONFLICT (attempt_id) DO NOTHING
RETURNING *;

-- name: TransactionForAttempt :one
SELECT * FROM authentication.transactions WHERE attempt_id = @attempt_id;

-- name: GetTransaction :one
SELECT * FROM authentication.transactions WHERE server_trans_id = @server_trans_id;

-- name: RecordResult :execrows
UPDATE authentication.transactions SET result_status = @result_status, updated_at = @now
WHERE server_trans_id = @server_trans_id AND result_status = '';
