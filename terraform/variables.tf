variable "yc_cloud_id" {
  description = "Yandex Cloud cloud ID"
  type        = string
}

variable "yc_folder_id" {
  description = "Yandex Cloud folder ID"
  type        = string
}

variable "yc_zone" {
  description = "Yandex Cloud zone"
  type        = string
  default     = "ru-central1-a"
}

variable "telegram_bot_token" {
  description = "Telegram bot token for the identity bot"
  type        = string
  sensitive   = true
}

variable "telegram_webhook_secret" {
  description = "Shared secret validated against the X-Telegram-Bot-Api-Secret-Token header"
  type        = string
  sensitive   = true
}

variable "admin_credential_encryption_key" {
  description = "AES-256-GCM key (base64) used to encrypt the admin's S21 password in bot_admin"
  type        = string
  sensitive   = true
}

variable "identity_service_url" {
  description = "Base URL of the identity service (placeholder-3)"
  type        = string
}

variable "identity_service_api_key" {
  description = "Identity-bot's own write-scope X-Api-Key for the identity service. Created via the admin CLI (`ttbot-identity-admin create-key --name identity-bot-prod --scopes write`). Optional pre-bootstrap — commands that need it fail with a clear message if unset."
  type        = string
  sensitive   = true
  default     = ""
}

variable "log_level" {
  description = "Function log verbosity (info, debug)."
  type        = string
  default     = "info"
}
