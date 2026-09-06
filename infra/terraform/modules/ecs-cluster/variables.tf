variable "name_prefix" {
  type = string
}

variable "ecr_kms_key_arn" {
  description = "Key for ECR image encryption (platform S3 key)."
  type        = string
}

variable "binaries" {
  description = "One immutable ECR repository per cmd/<name> (SYSTEM.md section 2)."
  type        = list(string)
  default = [
    "api", "relay-worker", "execution-worker", "reconciliation-worker", "market-ingest-worker",
    "agent-worker", "workflow-worker", "audit-worker", "migrate",
  ]
}

variable "ecr_keep_images" {
  description = "Images to keep per repository (release.yml tags with the git SHA)."
  type        = number
  default     = 30
}

variable "ecr_force_delete" {
  description = "Allow deleting repositories that still contain images (dev only)."
  type        = bool
  default     = false
}

variable "container_insights" {
  type    = string
  default = "enabled"
}

variable "tags" {
  type    = map(string)
  default = {}
}
