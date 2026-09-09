variable "name_prefix" {
  type = string
}

variable "aws_region" {
  type = string
}

variable "aws_account_id" {
  type = string
}

variable "github_org" {
  type = string
}

variable "github_repo" {
  type = string
}

variable "allowed_refs" {
  description = "Git refs whose workflows may assume the deploy role (main and release tags only)."
  type        = list(string)
  default     = ["refs/heads/main", "refs/tags/v*"]
}

variable "create_oidc_provider" {
  description = "Create the GitHub OIDC provider (one per account). false when another stack already owns it."
  type        = bool
  default     = true
}

variable "oidc_provider_arn" {
  description = "Existing provider ARN when create_oidc_provider = false."
  type        = string
  default     = null
}

variable "ecr_repository_arns" {
  type = list(string)
}

variable "ecs_cluster_arn" {
  type = string
}

variable "ecs_cluster_name" {
  type = string
}

variable "ecs_service_arns" {
  type = list(string)
}

variable "pass_role_arns" {
  description = "Task and execution roles the deploy role may pass to ECS."
  type        = list(string)
}

variable "migrate_task_definition_family" {
  type = string
}

variable "migrate_log_group_arn" {
  type = string
}

variable "tags" {
  type    = map(string)
  default = {}
}

# The ceiling on this role, not its permissions. The Terraform deployment
# identity may only create a role that carries it -- see
# infra/aws/nodal-task-boundary-policy.json and the condition on iam:CreateRole
# in nodal-terraform-iam-policy.json. Without it, a deployment identity that can
# create a role and write its inline policy can grant itself anything, which
# makes every other restriction on that identity decorative.
variable "permissions_boundary_arn" {
  type        = string
  description = "IAM permissions boundary applied to every role this module creates."
}
