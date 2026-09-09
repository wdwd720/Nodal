variable "name_prefix" {
  type = string
}

variable "service_name" {
  description = "Binary name: api, execution-worker, ..., migrate, db-bootstrap."
  type        = string
}

variable "aws_region" {
  type = string
}

variable "aws_account_id" {
  type = string
}

variable "cluster_arn" {
  type = string
}

variable "cluster_name" {
  type = string
}

variable "image" {
  description = "Immutable image reference (<ecr url>:<git sha> or @sha256:...)."
  type        = string
}

variable "cpu" {
  type = number
}

variable "memory" {
  type = number
}

variable "desired_count" {
  type    = number
  default = 1
}

variable "create_service" {
  description = "false for one-shot task definitions (migrate, db-bootstrap) that are started with ecs run-task."
  type        = bool
  default     = true
}

variable "subnet_ids" {
  description = "Private app subnets."
  type        = list(string)
}

variable "security_group_ids" {
  type = list(string)
}

variable "environment" {
  description = "CP_* variables (values are references, never secret material)."
  type        = map(string)
  default     = {}
}

variable "runtime_secret_arns" {
  description = "Secrets Manager ARNs the TASK role may resolve at runtime (aws-sm:// references)."
  type        = list(string)
  default     = []
}

variable "injected_secrets" {
  description = "Env var name -> Secrets Manager valueFrom (ARN[:json-key::]) injected by ECS with the EXECUTION role. Used only by db-bootstrap."
  type        = map(string)
  default     = {}
}

variable "secrets_kms_key_arn" {
  type = string
}

variable "ecr_repository_arns" {
  description = "Repositories the execution role may pull from; defaults to <name_prefix>/* in this account."
  type        = list(string)
  default     = []
}

variable "logs_kms_key_arn" {
  type = string
}

variable "log_retention_days" {
  type    = number
  default = 90
}

variable "container_port" {
  description = "Exposed port (api only)."
  type        = number
  default     = null
}

variable "target_group_arn" {
  description = "Primary ALB target group (readiness, /v1/readyz) for the api service; null for workers."
  type        = string
  default     = null
}

variable "extra_target_group_arns" {
  description = "Additional target groups the same container registers in (the liveness group on /v1/healthz). Ignored when target_group_arn is null."
  type        = list(string)
  default     = []
}

variable "autoscaling_max_capacity" {
  description = "Setting this (api only) puts Application Auto Scaling in charge of the task count and makes the service ignore desired_count afterwards. null leaves the count fixed at desired_count."
  type        = number
  default     = null
}

variable "autoscaling_min_capacity" {
  description = "Floor for the autoscaled task count. Must be >= 2 wherever an AZ may be lost without an outage."
  type        = number
  default     = 2
}

variable "autoscaling_cpu_target" {
  description = "Average service CPU the target-tracking policy holds."
  type        = number
  default     = 60
}

variable "autoscaling_requests_per_target" {
  description = "ALB requests per task the second policy holds. Only used when autoscaling_alb_resource_label is set."
  type        = number
  default     = 400
}

variable "autoscaling_alb_resource_label" {
  description = "ALBRequestCountPerTarget resource label: app/<alb name>/<alb id>/targetgroup/<tg name>/<tg id>. null disables the request-rate policy."
  type        = string
  default     = null
}

variable "autoscaling_scale_out_cooldown" {
  type    = number
  default = 60
}

variable "autoscaling_scale_in_cooldown" {
  description = "Deliberately long: shedding a task that is draining money-path requests is the expensive mistake."
  type        = number
  default     = 300
}

variable "health_check_grace_period_seconds" {
  type    = number
  default = 60
}

variable "command" {
  description = "Container command override (one-shot tasks)."
  type        = list(string)
  default     = []
}

variable "container_health_command" {
  description = "Optional ECS container health check command. Distroless images ship no shell, so this stays empty unless the binary gains a healthcheck subcommand; the api is health-checked by the ALB on /readyz."
  type        = list(string)
  default     = []
}

variable "container_user" {
  description = "Numeric uid:gid; the runtime image is distroless nonroot (build/Dockerfile)."
  type        = string
  default     = "65532:65532"
}

variable "readonly_root_filesystem" {
  type    = bool
  default = true
}

variable "cpu_architecture" {
  type    = string
  default = "X86_64"
}

variable "ephemeral_storage_gib" {
  type    = number
  default = 21
}

variable "stop_timeout_seconds" {
  description = "Grace period for in-flight work on SIGTERM (Fargate max 120)."
  type        = number
  default     = 60
}

variable "deployment_minimum_healthy_percent" {
  type    = number
  default = 100
}

variable "deployment_maximum_percent" {
  type    = number
  default = 200
}

variable "task_policy_statements" {
  description = "Extra Allow statements for the task role (S3 evidence, KMS sign, ...)."
  type = list(object({
    sid       = string
    actions   = list(string)
    resources = list(string)
  }))
  default = []
}

variable "otel_sidecar_enabled" {
  description = "Run the ADOT collector as a sidecar listening on 127.0.0.1:4317. NOTE: config.Validate rejects insecure OTLP in PROD, so prod points at a TLS collector instead (DEPLOYMENT.md section 7)."
  type        = bool
  default     = false
}

variable "otel_collector_image" {
  type    = string
  default = "public.ecr.aws/aws-observability/aws-otel-collector:v0.42.0"
}

variable "otel_config_path" {
  description = "Built-in ADOT config to use (CloudWatch EMF metrics + X-Ray traces)."
  type        = string
  default     = "/etc/ecs/ecs-cloudwatch-xray.yaml"
}

variable "otel_metric_namespace" {
  type    = string
  default = "ECS/AWSOTel/Application"
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
