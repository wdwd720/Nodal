variable "name_prefix" {
  description = "Resource name prefix, e.g. cp-prod."
  type        = string
}

variable "aws_account_id" {
  description = "Account that owns the keys (used in key policies)."
  type        = string
}

variable "aws_region" {
  description = "Region of the keys (used for service-principal conditions)."
  type        = string
}

variable "audit_signer_role_arns" {
  description = "IAM role ARNs allowed kms:Sign on the audit checkpoint key. Must contain ONLY the audit-worker task role (PART 100)."
  type        = list(string)
}

variable "audit_verifier_principal_arns" {
  description = "Principals (auditor roles/users) allowed kms:Verify and kms:GetPublicKey on the audit checkpoint key."
  type        = list(string)
  default     = []
}

variable "key_admin_principal_arns" {
  description = "Additional principals granted key administration (never Sign/Decrypt on the audit key)."
  type        = list(string)
  default     = []
}

variable "deletion_window_in_days" {
  description = "Pending-deletion window for every key."
  type        = number
  default     = 30
}

variable "tags" {
  type    = map(string)
  default = {}
}
