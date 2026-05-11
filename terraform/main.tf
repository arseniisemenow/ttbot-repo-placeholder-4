terraform {
  required_version = ">= 1.0"

  required_providers {
    yandex = {
      source  = "yandex-cloud/yandex"
      version = "~> 0.113.0"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.0"
    }
  }
}

data "external" "iam_token" {
  program = ["bash", "-c", "echo '{\"token\":\"'$(yc iam create-token 2>/dev/null)'\"}'"]
}

provider "yandex" {
  cloud_id  = var.yc_cloud_id
  folder_id = var.yc_folder_id
  zone      = var.yc_zone
  token     = data.external.iam_token.result["token"]
}

resource "yandex_iam_service_account" "fn_sa" {
  name        = "identity-bot-fn-sa"
  description = "Service account for the identity-bot webhook function"
}

resource "yandex_resourcemanager_folder_iam_member" "fn_sa_ydb_editor" {
  folder_id = var.yc_folder_id
  role      = "ydb.editor"
  member    = "serviceAccount:${yandex_iam_service_account.fn_sa.id}"
}

data "archive_file" "function" {
  type        = "zip"
  source_dir  = "${path.module}/function"
  output_path = "${path.module}/function.zip"
  excludes    = ["function", "function.zip", ".gitkeep"]
}

resource "yandex_function" "identity_bot" {
  name               = "identity-bot-webhook"
  description        = "Identity bot Telegram webhook"
  user_hash          = data.archive_file.function.output_base64sha256
  runtime            = "golang123"
  entrypoint         = "handler.Handler"
  memory             = 256
  execution_timeout  = 30
  service_account_id = yandex_iam_service_account.fn_sa.id

  content {
    zip_filename = data.archive_file.function.output_path
  }

  environment = {
    TELEGRAM_BOT_TOKEN              = var.telegram_bot_token
    TELEGRAM_WEBHOOK_SECRET         = var.telegram_webhook_secret
    ADMIN_CREDENTIAL_ENCRYPTION_KEY = var.admin_credential_encryption_key
    IDENTITY_SERVICE_URL            = var.identity_service_url
    YDB_ENDPOINT                    = yandex_ydb_database_serverless.db.ydb_full_endpoint
    YDB_AUTH_METADATA               = "true"
    LOG_LEVEL                       = var.log_level
  }
}

resource "yandex_function_iam_binding" "public_invoker" {
  function_id = yandex_function.identity_bot.id
  role        = "serverless.functions.invoker"
  members     = ["system:allUsers"]
}

resource "yandex_api_gateway" "gateway" {
  name        = "identity-bot-gateway"
  description = "API Gateway fronting the identity-bot webhook"
  labels = {
    bot = "identity-bot"
  }

  spec = <<-EOF
    openapi: "3.0.0"
    info:
      title: "Identity Bot Webhook"
      version: "1.0"
    paths:
      /:
        post:
          x-yc-apigateway-integration:
            type: cloud_functions
            function_id: ${yandex_function.identity_bot.id}
            service_account_id: ${yandex_iam_service_account.fn_sa.id}
          operationId: handleWebhook
  EOF
}
