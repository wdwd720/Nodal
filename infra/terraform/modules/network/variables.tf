variable "name_prefix" {
  type = string
}

variable "aws_region" {
  type = string
}

variable "vpc_cidr" {
  description = "VPC CIDR (a /16 is split into /20 subnets: 3 public, 3 private-app, 3 private-data)."
  type        = string
}

variable "availability_zones" {
  description = "Exactly three availability zones."
  type        = list(string)
  validation {
    condition     = length(var.availability_zones) == 3
    error_message = "Three availability zones are required (PART 138/139: Multi-AZ)."
  }
}

variable "single_nat_gateway" {
  description = "true = one NAT gateway (dev cost saving); false = one per AZ (staging/prod)."
  type        = bool
}

variable "logs_kms_key_arn" {
  description = "KMS key for the VPC flow log group."
  type        = string
}

variable "flow_log_retention_days" {
  type    = number
  default = 90
}

variable "api_container_port" {
  type    = number
  default = 8080
}

variable "alb_ingress_cidrs" {
  description = "CIDRs allowed to reach the ALB on 443/80 when CloudFront is not in front."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "alb_ingress_from_cloudfront_only" {
  description = "When true, the ALB accepts 443 only from the CloudFront origin-facing managed prefix list."
  type        = bool
  default     = false
}

variable "interface_endpoints" {
  description = "Interface VPC endpoints (S3 is always a gateway endpoint)."
  type        = list(string)
  default     = ["ecr.api", "ecr.dkr", "secretsmanager", "kms", "logs", "monitoring", "sts"]
}

variable "tags" {
  type    = map(string)
  default = {}
}
