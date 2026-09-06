variable "name_prefix" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "public_subnet_ids" {
  type = list(string)
}

variable "alb_security_group_id" {
  type = string
}

variable "acm_certificate_arn" {
  description = "Regional ACM certificate for the ALB listener."
  type        = string
}

variable "target_port" {
  type    = number
  default = 8080
}

variable "health_check_path" {
  description = "Readiness probe. internal/httpapi mounts every route under BasePath = \"/v1\", so this must carry the prefix: an unprefixed /readyz answers 404 and the ALB never brings a target into service."
  type        = string
  default     = "/v1/readyz"
}

variable "liveness_check_path" {
  description = "Liveness probe, answered 200 by the process itself. Kept on a second target group so 'the process is wedged' and 'a dependency is down' are two different alarms."
  type        = string
  default     = "/v1/healthz"
}

variable "bucket_suffix" {
  description = "Globally-unique suffix for the ALB access-log bucket (<account>-<region>)."
  type        = string
}

variable "alb_log_retention_days" {
  type    = number
  default = 90
}

variable "logs_kms_key_arn" {
  description = "Key for the WAF log group."
  type        = string
}

variable "waf_log_retention_days" {
  type    = number
  default = 90
}

variable "waf_rate_limit" {
  description = "Requests per 5 minutes per IP before the rate-based rule blocks."
  type        = number
  default     = 2000
}

variable "deletion_protection" {
  type = bool
}

variable "idle_timeout" {
  type    = number
  default = 60
}

variable "cloudfront_enabled" {
  description = "Put CloudFront (with a CLOUDFRONT-scope WAF) in front of the ALB; the ALB then only accepts CloudFront traffic carrying the origin-verify header."
  type        = bool
  default     = false
}

variable "cloudfront_acm_certificate_arn" {
  description = "ACM certificate in us-east-1 for the CloudFront aliases."
  type        = string
  default     = null
}

variable "cloudfront_aliases" {
  type    = list(string)
  default = []
}

variable "cloudfront_origin_domain_name" {
  description = "DNS name (covered by the ALB certificate) that resolves to the ALB; CloudFront validates the origin certificate against it."
  type        = string
  default     = null
}

variable "cloudfront_origin_secret" {
  description = "Shared secret CloudFront adds as x-origin-verify; the regional WAF blocks requests without it."
  type        = string
  default     = null
  sensitive   = true
}

variable "force_destroy_logs" {
  type    = bool
  default = false
}

variable "tags" {
  type    = map(string)
  default = {}
}
