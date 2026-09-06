# Network (PART 102): private RDS/Redis/workers, public entry only through the
# ALB subnets, NAT for outbound provider calls, VPC endpoints for AWS APIs,
# restricted security groups, no management ports, flow logs.

locals {
  az_count            = length(var.availability_zones)
  public_subnet_cidrs = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i)]
  app_subnet_cidrs    = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i + 3)]
  data_subnet_cidrs   = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i + 6)]
  nat_count           = var.single_nat_gateway ? 1 : local.az_count
}

resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = merge(var.tags, { Name = "${var.name_prefix}-vpc" })
}

# The default security group is left with no rules so nothing can use it by accident.
resource "aws_default_security_group" "this" {
  vpc_id = aws_vpc.this.id
  tags   = merge(var.tags, { Name = "${var.name_prefix}-default-unused" })
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = merge(var.tags, { Name = "${var.name_prefix}-igw" })
}

# ---------------------------------------------------------------------------
# Subnets
# ---------------------------------------------------------------------------

resource "aws_subnet" "public" {
  count                   = local.az_count
  vpc_id                  = aws_vpc.this.id
  cidr_block              = local.public_subnet_cidrs[count.index]
  availability_zone       = var.availability_zones[count.index]
  map_public_ip_on_launch = false
  tags                    = merge(var.tags, { Name = "${var.name_prefix}-public-${var.availability_zones[count.index]}", Tier = "public" })
}

resource "aws_subnet" "private_app" {
  count             = local.az_count
  vpc_id            = aws_vpc.this.id
  cidr_block        = local.app_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]
  tags              = merge(var.tags, { Name = "${var.name_prefix}-app-${var.availability_zones[count.index]}", Tier = "private-app" })
}

resource "aws_subnet" "private_data" {
  count             = local.az_count
  vpc_id            = aws_vpc.this.id
  cidr_block        = local.data_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]
  tags              = merge(var.tags, { Name = "${var.name_prefix}-data-${var.availability_zones[count.index]}", Tier = "private-data" })
}

# ---------------------------------------------------------------------------
# Routing: public -> IGW; app -> NAT; data -> no default route (no egress)
# ---------------------------------------------------------------------------

resource "aws_eip" "nat" {
  count  = local.nat_count
  domain = "vpc"
  tags   = merge(var.tags, { Name = "${var.name_prefix}-nat-${count.index}" })
}

resource "aws_nat_gateway" "this" {
  count         = local.nat_count
  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id
  tags          = merge(var.tags, { Name = "${var.name_prefix}-nat-${var.availability_zones[count.index]}" })
  depends_on    = [aws_internet_gateway.this]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  tags   = merge(var.tags, { Name = "${var.name_prefix}-rt-public" })
}

resource "aws_route" "public_default" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this.id
}

resource "aws_route_table_association" "public" {
  count          = local.az_count
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private_app" {
  count  = local.az_count
  vpc_id = aws_vpc.this.id
  tags   = merge(var.tags, { Name = "${var.name_prefix}-rt-app-${var.availability_zones[count.index]}" })
}

resource "aws_route" "private_app_default" {
  count                  = local.az_count
  route_table_id         = aws_route_table.private_app[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this[var.single_nat_gateway ? 0 : count.index].id
}

resource "aws_route_table_association" "private_app" {
  count          = local.az_count
  subnet_id      = aws_subnet.private_app[count.index].id
  route_table_id = aws_route_table.private_app[count.index].id
}

resource "aws_route_table" "private_data" {
  vpc_id = aws_vpc.this.id
  tags   = merge(var.tags, { Name = "${var.name_prefix}-rt-data" })
}

resource "aws_route_table_association" "private_data" {
  count          = local.az_count
  subnet_id      = aws_subnet.private_data[count.index].id
  route_table_id = aws_route_table.private_data.id
}

# ---------------------------------------------------------------------------
# VPC endpoints: S3 gateway + interface endpoints for ECR, Secrets Manager,
# KMS, CloudWatch Logs (and monitoring/STS) so AWS API traffic never leaves
# the VPC.
# ---------------------------------------------------------------------------

resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${var.aws_region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = concat(aws_route_table.private_app[*].id, [aws_route_table.private_data.id])
  tags              = merge(var.tags, { Name = "${var.name_prefix}-vpce-s3" })
}

resource "aws_vpc_endpoint" "interface" {
  for_each            = toset(var.interface_endpoints)
  vpc_id              = aws_vpc.this.id
  service_name        = "com.amazonaws.${var.aws_region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = aws_subnet.private_app[*].id
  security_group_ids  = [aws_security_group.vpce.id]
  private_dns_enabled = true
  tags                = merge(var.tags, { Name = "${var.name_prefix}-vpce-${replace(each.value, ".", "-")}" })
}

# ---------------------------------------------------------------------------
# Security groups. Rules are separate resources so every rule has a
# description and a single responsibility.
# ---------------------------------------------------------------------------

resource "aws_security_group" "alb" {
  name        = "${var.name_prefix}-alb"
  description = "Public ALB: 443 in from the edge, 8080 out to the api service only"
  vpc_id      = aws_vpc.this.id
  tags        = merge(var.tags, { Name = "${var.name_prefix}-alb" })
}

resource "aws_security_group" "api" {
  name        = "${var.name_prefix}-api"
  description = "api tasks: ingress only from the ALB; egress to RDS, Redis, VPC endpoints and HTTPS providers"
  vpc_id      = aws_vpc.this.id
  tags        = merge(var.tags, { Name = "${var.name_prefix}-api" })
}

resource "aws_security_group" "worker" {
  name        = "${var.name_prefix}-worker"
  description = "worker tasks: no ingress; egress to RDS, Redis, VPC endpoints and HTTPS providers"
  vpc_id      = aws_vpc.this.id
  tags        = merge(var.tags, { Name = "${var.name_prefix}-worker" })
}

resource "aws_security_group" "db" {
  name        = "${var.name_prefix}-db"
  description = "RDS: 5432 only from api and worker tasks"
  vpc_id      = aws_vpc.this.id
  tags        = merge(var.tags, { Name = "${var.name_prefix}-db" })
}

resource "aws_security_group" "redis" {
  name        = "${var.name_prefix}-redis"
  description = "ElastiCache: 6379 only from api and worker tasks"
  vpc_id      = aws_vpc.this.id
  tags        = merge(var.tags, { Name = "${var.name_prefix}-redis" })
}

resource "aws_security_group" "vpce" {
  name        = "${var.name_prefix}-vpce"
  description = "Interface VPC endpoints: 443 from api and worker tasks"
  vpc_id      = aws_vpc.this.id
  tags        = merge(var.tags, { Name = "${var.name_prefix}-vpce" })
}

# --- ALB ---
data "aws_ec2_managed_prefix_list" "cloudfront" {
  count = var.alb_ingress_from_cloudfront_only ? 1 : 0
  name  = "com.amazonaws.global.cloudfront.origin-facing"
}

#trivy:ignore:AVD-AWS-0107 the ALB is the single controlled public entry (PART 102); it sits behind WAFv2 and only forwards to the api service
resource "aws_vpc_security_group_ingress_rule" "alb_https_public" {
  for_each          = var.alb_ingress_from_cloudfront_only ? toset([]) : toset(var.alb_ingress_cidrs)
  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from the public edge"
  cidr_ipv4         = each.value
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
  tags              = var.tags
}

resource "aws_vpc_security_group_ingress_rule" "alb_https_cloudfront" {
  count             = var.alb_ingress_from_cloudfront_only ? 1 : 0
  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from CloudFront origin-facing addresses only"
  prefix_list_id    = data.aws_ec2_managed_prefix_list.cloudfront[0].id
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
  tags              = var.tags
}

# Port 80 is accepted only to redirect to 443 (listener in waf-edge).
#trivy:ignore:AVD-AWS-0107 port 80 exists solely for the HTTPS redirect listener
resource "aws_vpc_security_group_ingress_rule" "alb_http_redirect" {
  for_each          = var.alb_ingress_from_cloudfront_only ? toset([]) : toset(var.alb_ingress_cidrs)
  security_group_id = aws_security_group.alb.id
  description       = "HTTP, redirected to HTTPS by the listener"
  cidr_ipv4         = each.value
  from_port         = 80
  to_port           = 80
  ip_protocol       = "tcp"
  tags              = var.tags
}

resource "aws_vpc_security_group_egress_rule" "alb_to_api" {
  security_group_id            = aws_security_group.alb.id
  description                  = "To api tasks only"
  referenced_security_group_id = aws_security_group.api.id
  from_port                    = var.api_container_port
  to_port                      = var.api_container_port
  ip_protocol                  = "tcp"
  tags                         = var.tags
}

# --- api ---
resource "aws_vpc_security_group_ingress_rule" "api_from_alb" {
  security_group_id            = aws_security_group.api.id
  description                  = "From the ALB only"
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = var.api_container_port
  to_port                      = var.api_container_port
  ip_protocol                  = "tcp"
  tags                         = var.tags
}

# --- api + worker shared egress ---
locals {
  app_sgs = {
    api    = aws_security_group.api.id
    worker = aws_security_group.worker.id
  }
}

resource "aws_vpc_security_group_egress_rule" "app_to_db" {
  for_each                     = local.app_sgs
  security_group_id            = each.value
  description                  = "PostgreSQL to RDS"
  referenced_security_group_id = aws_security_group.db.id
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
  tags                         = var.tags
}

resource "aws_vpc_security_group_egress_rule" "app_to_redis" {
  for_each                     = local.app_sgs
  security_group_id            = each.value
  description                  = "Redis (TLS) to ElastiCache"
  referenced_security_group_id = aws_security_group.redis.id
  from_port                    = 6379
  to_port                      = 6379
  ip_protocol                  = "tcp"
  tags                         = var.tags
}

resource "aws_vpc_security_group_egress_rule" "app_to_vpce" {
  for_each                     = local.app_sgs
  security_group_id            = each.value
  description                  = "HTTPS to interface VPC endpoints"
  referenced_security_group_id = aws_security_group.vpce.id
  from_port                    = 443
  to_port                      = 443
  ip_protocol                  = "tcp"
  tags                         = var.tags
}

#trivy:ignore:AVD-AWS-0104 outbound HTTPS via NAT is required to reach external providers (Jupiter, Helius, wallet/signing, Stripe, model, Temporal/Redpanda/ClickHouse Cloud); restricted to tcp/443
resource "aws_vpc_security_group_egress_rule" "app_https_out" {
  for_each          = local.app_sgs
  security_group_id = each.value
  description       = "HTTPS to external providers and the S3 gateway endpoint"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
  tags              = var.tags
}

# --- data tier ---
resource "aws_vpc_security_group_ingress_rule" "db_from_app" {
  for_each                     = local.app_sgs
  security_group_id            = aws_security_group.db.id
  description                  = "PostgreSQL from ${each.key} tasks"
  referenced_security_group_id = each.value
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
  tags                         = var.tags
}

resource "aws_vpc_security_group_ingress_rule" "redis_from_app" {
  for_each                     = local.app_sgs
  security_group_id            = aws_security_group.redis.id
  description                  = "Redis from ${each.key} tasks"
  referenced_security_group_id = each.value
  from_port                    = 6379
  to_port                      = 6379
  ip_protocol                  = "tcp"
  tags                         = var.tags
}

resource "aws_vpc_security_group_ingress_rule" "vpce_from_app" {
  for_each                     = local.app_sgs
  security_group_id            = aws_security_group.vpce.id
  description                  = "HTTPS from ${each.key} tasks"
  referenced_security_group_id = each.value
  from_port                    = 443
  to_port                      = 443
  ip_protocol                  = "tcp"
  tags                         = var.tags
}

# ---------------------------------------------------------------------------
# Flow logs
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "flow" {
  name              = "/aws/vpc/${var.name_prefix}/flow-logs"
  retention_in_days = var.flow_log_retention_days
  kms_key_id        = var.logs_kms_key_arn
  tags              = var.tags
}

data "aws_iam_policy_document" "flow_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["vpc-flow-logs.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "flow" {
  statement {
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
    resources = ["${aws_cloudwatch_log_group.flow.arn}:*"]
  }
}

resource "aws_iam_role" "flow" {
  name               = "${var.name_prefix}-vpc-flow-logs"
  assume_role_policy = data.aws_iam_policy_document.flow_assume.json
  tags               = var.tags
}

resource "aws_iam_role_policy" "flow" {
  name   = "flow-logs"
  role   = aws_iam_role.flow.id
  policy = data.aws_iam_policy_document.flow.json
}

resource "aws_flow_log" "this" {
  vpc_id          = aws_vpc.this.id
  traffic_type    = "ALL"
  iam_role_arn    = aws_iam_role.flow.arn
  log_destination = aws_cloudwatch_log_group.flow.arn
  tags            = merge(var.tags, { Name = "${var.name_prefix}-flow" })
}
