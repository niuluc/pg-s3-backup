# Manual testing

Use this flow to accept pg-backup in production. It runs next to the
existing `pg-s3-backup` sidecars. Nothing is removed during this flow.

## 1. Reach it

1. On a device on the tailnet, open `https://pg-backup.<tailnet>.ts.net`.
   - Expect: the page opens. The header shows your Tailscale login.
2. Open the same URL from a device that is not on the tailnet.
   - Expect: the name does not resolve and nothing loads.

## 2. Add a database

1. Click **Add database**. Fill in the values from the database's current
   sidecar. Use the same S3 folder as that sidecar (`S3_PREFIX`).
2. Click **Save and test connection**.
   - Expect: "Saved. Connected to Postgres <version>." The version matches
     the server.

## 3. Back up and verify

1. On the database page, click **Back up now**.
   - Expect: the run page shows a live log and ends with `backup complete`.
     The log names the client, for example `postgresql16`.
2. Click **Verify latest**.
   - Expect: `verify ok: ... restored with N tables`. N is greater than 0.
3. On the database page, look at **Backups in S3**.
   - Expect: the new file is at the top, next to the sidecar's files.

## 4. Restore as copy

1. Next to a backup, click **Restore…**, then click **Restore as copy**.
   - Expect: the run ends with `restored into <database>_restore_<time>`.
2. Connect to that database and check a table you know.
3. Drop the copy when you are done.

## 5. Schedule

1. Wait for the next scheduled time.
   - Expect: a run with trigger `schedule` and status ok. On the main page
     the dot is green and "Last backup" shows the new age.

## 6. Failure shows

Use a disposable local test database for this check. Do not change a production
password to test notifications. A live Slack message requires separate send approval.

1. Configure `SLACK_BOT_TOKEN` and `SLACK_CHANNEL` for an approved test channel,
   then edit the test database. Set a wrong password. Click **Back up now**.
   - Expect: the run fails. On the main page the dot is red and the row
     reads "failed". The configured Slack channel gets one message with the
     job kind, database name, trigger and error. Without both Slack settings,
     no message is sent.
2. Set the right password again and click **Back up now**.
   - Expect: the run succeeds. No success or recovery message is sent.

Run `go test -run TestSlackNotifier -v ./...` from `app/` to check request
content, Slack API errors and safe logs against a local `httptest` server.
This sends no live messages.

Replace live is covered by `test/e2e.sh`. Try it in production only when
you need it.
