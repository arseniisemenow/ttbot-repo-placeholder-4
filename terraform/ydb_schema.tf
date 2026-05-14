resource "yandex_ydb_database_serverless" "db" {
  name                = "identity-bot-db"
  deletion_protection = false

  labels = {
    project = "identity-bot"
  }
}

# bot_admin — single-row table holding the current admin's encrypted S21
# creds. Last-wins via the /admin command.
resource "yandex_ydb_table" "bot_admin" {
  path              = "bot_admin"
  connection_string = yandex_ydb_database_serverless.db.ydb_full_endpoint

  column {
    name     = "id"
    type     = "Uint8"
    not_null = true
  }
  column {
    name     = "telegram_id"
    type     = "Uint64"
    not_null = true
  }
  column {
    name     = "s21_login"
    type     = "Utf8"
    not_null = true
  }
  column {
    name     = "s21_creds_encrypted"
    type     = "Utf8"
    not_null = true
  }
  column {
    name     = "updated_at"
    type     = "Timestamp"
    not_null = true
  }
  # Nullable: timestamp of the FIRST failed S21 re-auth in the current
  # failure run. Cleared on next successful auth. Drives the 7-day
  # auto-unadmin clock and the 4-DM warning cadence.
  column {
    name     = "s21_creds_failed_at"
    type     = "Timestamp"
    not_null = false
  }
  # Nullable: when the cron last sent a failure-warning DM. Used to
  # suppress duplicate warnings between milestones.
  column {
    name     = "s21_creds_last_warned_at"
    type     = "Timestamp"
    not_null = false
  }

  primary_key = ["id"]
}

# pending_deletes — messages the bot has DMed that need to vanish later. The
# cron function sweeps rows whose delete_at is in the past and removes them
# both from Telegram and from this table.
resource "yandex_ydb_table" "pending_deletes" {
  path              = "pending_deletes"
  connection_string = yandex_ydb_database_serverless.db.ydb_full_endpoint

  column {
    name     = "chat_id"
    type     = "Int64"
    not_null = true
  }
  column {
    name     = "message_id"
    type     = "Int64"
    not_null = true
  }
  column {
    name     = "delete_at"
    type     = "Timestamp"
    not_null = true
  }
  column {
    name     = "created_at"
    type     = "Timestamp"
    not_null = true
  }

  primary_key = ["chat_id", "message_id"]
}
