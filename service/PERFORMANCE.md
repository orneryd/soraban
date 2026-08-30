# Local Performance Sample

Date: 2026-08-30
Machine: Apple silicon macOS development machine
Database: PostgreSQL 18.4 Alpine in Docker, durable defaults enabled
Execution: F001 and F002 processed concurrently, one bounded streaming pipeline per firm

| Firm |    Rows | Import | Determination | Batch planning | Batches |
| ---- | ------: | -----: | ------------: | -------------: | ------: |
| F001 | 500,000 | 11.73s |         3.43s |          3.98s |     998 |
| F002 | 500,000 | 11.58s |         3.45s |          3.99s |   1,013 |

The complete fresh-database E2E, including checksum verification and replay of
both files, completed in 22.07 seconds wall time.

Implementation notes:

- each gzip/CSV file is pulled record-by-record directly into PostgreSQL COPY;
- firms run concurrently because their datasets and rate budgets are independent;
- determination uses one set-based aggregate/insert statement per firm; and
- planning generates stable XML in Go, bulk loads batch/link staging rows with
  COPY, then inserts links and transitions filings set-wise.

Run with:

```sh
cd service
make test-e2e
```
