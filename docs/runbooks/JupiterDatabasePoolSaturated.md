# JupiterDatabasePoolSaturated

**Warning.** Metrics: `jupiter_db_pool_waited_acquires_total` over
`jupiter_db_pool_acquires_total`, by service: over a fifth of the connections taken from
the pool for 10 minutes had to wait for one.

## What it means

Every connection in the service's pool is busy. The pool's size is pgx's default (the
host's CPUs, at least 4) unless `JUPITER_DATABASE_URL` sets `pool_max_conns`. Either the
pool is small for the traffic, or the database is slow, so each request holds its
connection longer.

## Impact

Requests queue for a connection, and latency rises with the queue.

## Diagnose

- Connections in use against the maximum: `jupiter_db_pool_connections` by `state`.
- What the database is doing:
  ```sql
  SELECT state, wait_event_type, wait_event, count(*) FROM pg_stat_activity
  WHERE datname = current_database() GROUP BY 1, 2, 3 ORDER BY 4 DESC;
  ```
  Many `idle in transaction` sessions mean the service holds connections between
  statements, waiting on something else, such as a rail. Many `active` sessions waiting
  on locks or I/O mean the database is the limit.
- The benchmark found that a larger pool did not raise throughput once the database was
  the limit ([authorization path](../benchmarks/authorization-path.md)).

## Fix

- If the database has headroom, raise `pool_max_conns`, within PostgreSQL's
  `max_connections` across every service.
- If the database is the limit, a larger pool only moves the queue into PostgreSQL. Find
  the slow statements, the hot rows or the disk first.
