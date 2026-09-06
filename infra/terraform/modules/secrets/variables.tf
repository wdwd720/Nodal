variable "secret_name_prefix" {
  description = "Secrets Manager name prefix, e.g. cp/prod. Every aws-sm:// reference in the task environment is <prefix>/<name>."
  type        = string
}

variable "kms_key_arn" {
  description = "Platform secrets key."
  type        = string
}

variable "task_role_arns" {
  description = "Map of service key (api, execution-worker, ..., migrate, db-bootstrap) to the IAM role that resolves secrets for it. A service missing from the map simply gets no access (fail closed)."
  type        = map(string)
}

variable "admin_principal_arns" {
  description = "Operator/break-glass principals that may read and write secret values (populate them after bootstrap). Rotation Lambda roles belong here too."
  type        = list(string)
  default     = []
}

variable "managed_secret_keys" {
  description = "Names of the secrets whose values Terraform writes itself (e.g. redis/url). Kept separate from the values so for_each never iterates a sensitive value."
  type        = list(string)
  default     = []
}

variable "managed_secret_values" {
  description = "Values for managed_secret_keys. Everything else is populated by operators (DEPLOYMENT.md section 4)."
  type        = map(string)
  default     = {}
  sensitive   = true
}

variable "rotation" {
  description = "Optional rotation hooks: secret name -> Lambda ARN and cadence."
  type = map(object({
    lambda_arn               = string
    automatically_after_days = number
  }))
  default = {}
}

variable "recovery_window_in_days" {
  type    = number
  default = 30
}

variable "tags" {
  type    = map(string)
  default = {}
}
