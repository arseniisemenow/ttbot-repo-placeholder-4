# ttbot-identity-bot

Telegram bot for users to register their S21 nicknames. Stores ONE row
(`bot_admin`) with encrypted S21 creds of whoever last ran `/admin`; uses
those creds to call [ttbot-identity-service](https://github.com/arseniisemenow/ttbot-repo-placeholder-3)
on behalf of any user who DMs `/provide_nickname`.

## Commands

| Command | Description |
|---|---|
| `/start`, `/help` | Greet and explain available commands. |
| `/admin <login>:<password>` | Claim the admin role (last-wins). Validates against S21. |
| `/provide_nickname <s21_login>` | Register your S21 nickname. |
| `/remove_nickname` | Clear your registered nickname. |
| `/my_nickname` | Show your stored nickname. |
| `/new_read_key <name>` | Mint a read-only API key for the identity service. Two-step force-reply flow (see below). |
| `/revoke_read_key <name>` | Revoke a read key you created. Same two-step flow. |
| `/list_users` | Admin-only. (Not yet implemented — service exposes lookups, not lists.) |

All commands DM-only; group-chat messages are silently ignored.

## `/new_read_key` flow

```
You:  /new_read_key alice-dashboard
Bot:  [KEY_OP=new name=alice-dashboard]
      Reply with your S21 credentials as `login:password` ...
You:  alice:hunter2          ← sent as a reply to the bot's prompt
Bot:  (deletes your creds message immediately)
Bot:  KEY CREATED — copy this NOW. It will vanish in about 15 minutes ...
      name:   alice-dashboard
      scopes: read
      key:    <plaintext>
```

The bot:

1. Validates your `login:password` against S21 — fresh, no caching.
2. Calls identity-service `POST /admin/keys` to mint the row.
3. DMs the plaintext key.
4. **Deletes your credentials message** as soon as it has read it.
5. Schedules the key DM for deletion ~15 minutes later. The cron sweeps
   the `pending_deletes` table and removes due messages.

You can have only ONE active read key per Telegram account; mint a
second one and the bot replies "revoke first".

## `/revoke_read_key` flow

Mirror of the above. Validates creds, calls identity-service `DELETE
/admin/keys/{name}?created_by={your-telegram-id}`. You can only revoke
keys you yourself created.

## Cron

`terraform/cron-function/` is a separate Cloud Function fired by a 15-min
timer trigger. Two jobs per tick:

1. **Re-validate primary admin's S21 creds.** Pulls the row from
   `bot_admin`, decrypts the stored password, calls `S21.Authenticate`.
   On `ErrInvalidCredentials` (password rotated, account disabled), DMs
   the admin: "S21 rejected your stored credentials ... re-run /admin".
2. **Sweep `pending_deletes`.** Deletes every row whose `delete_at` is
   in the past, both from Telegram and from the table.

## Architecture

- Two Cloud Functions: webhook (Telegram updates) and cron (periodic job).
- One YDB serverless instance: `bot_admin`, `pending_deletes`.
- HTTP client to identity-service uses `IDENTITY_SERVICE_API_KEY` (the bot's
  own write-scope key) + the calling user's just-validated S21 creds in
  `X-S21-Token`.

## Env vars (set via terraform.tfvars)

| Variable | Required | Purpose |
|---|---|---|
| `telegram_bot_token` | yes | Bot API token from @BotFather |
| `telegram_webhook_secret` | yes | Shared secret in the Telegram webhook header |
| `admin_credential_encryption_key` | yes | base64-encoded AES-256 key for `bot_admin.s21_creds_encrypted` |
| `identity_service_url` | yes | Base URL of the identity service |
| `identity_service_api_key` | bootstrap-optional | Write-scope X-Api-Key the bot uses to call /admin/keys. Mint via the admin CLI, paste here, redeploy. |

## Deploy

```sh
cd terraform
terraform plan
terraform apply
```

Register webhook with Telegram (one time):

```sh
curl -X POST "https://api.telegram.org/bot$BOT_TOKEN/setWebhook" \
  -H 'Content-Type: application/json' \
  -d '{"url":"'"$(terraform output -raw webhook_url)"'","secret_token":"'"$WEBHOOK_SECRET"'","drop_pending_updates":true}'
```

### Bootstrap order

If you're deploying this bot for the first time alongside the new
identity-service API-key auth:

1. Deploy identity-service in dry-run mode (its README explains).
2. Mint `identity-bot-prod` (write scope) via the identity-service admin
   CLI.
3. Paste the plaintext into `identity_service_api_key` in this repo's
   `terraform.tfvars`.
4. `terraform apply` here.
5. Verify `/new_read_key` works end-to-end.
6. Flip identity-service to enforcing mode.
