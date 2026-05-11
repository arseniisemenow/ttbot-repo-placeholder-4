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
| `/list_users` | Admin-only. (Not yet implemented — service exposes lookups, not lists.) |

All commands DM-only; group-chat messages are silently ignored.

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
