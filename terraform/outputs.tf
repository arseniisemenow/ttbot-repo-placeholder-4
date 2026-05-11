output "ydb_endpoint" {
  description = "YDB endpoint"
  value       = yandex_ydb_database_serverless.db.ydb_full_endpoint
}

output "function_id" {
  description = "Cloud Function ID"
  value       = yandex_function.identity_bot.id
}

output "gateway_id" {
  description = "API Gateway ID"
  value       = yandex_api_gateway.gateway.id
}

output "webhook_url" {
  description = "Webhook URL to register with Telegram"
  value       = "https://${yandex_api_gateway.gateway.domain}/"
}
