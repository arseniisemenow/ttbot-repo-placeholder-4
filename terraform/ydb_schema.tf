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
