# One Fargate service (or one-shot task definition) per binary, with its own
# task role and execution role (PART 100), hardened container settings
# (PART 103: non-root, read-only root filesystem, all capabilities dropped,
# resource limits, log retention) and a deployment circuit breaker with
# automatic rollback.

locals {
  full_name = "${var.name_prefix}-${var.service_name}"

  env_list      = [for k in sort(keys(var.environment)) : { name = k, value = var.environment[k] }]
  injected_list = [for k in sort(keys(var.injected_secrets)) : { name = k, valueFrom = var.injected_secrets[k] }]

  # "arn:aws:secretsmanager:region:acct:secret:name-xxxx[:json-key::]" -> secret ARN
  injected_secret_arns = distinct([
    for v in values(var.injected_secrets) : join(":", slice(split(":", v), 0, 7))
  ])

  ecr_repository_arns = length(var.ecr_repository_arns) > 0 ? var.ecr_repository_arns : [
    "arn:aws:ecr:${var.aws_region}:${var.aws_account_id}:repository/${var.name_prefix}/*"
  ]

  app_container = merge(
    {
      name                   = var.service_name
      image                  = var.image
      essential              = true
      user                   = var.container_user
      readonlyRootFilesystem = var.readonly_root_filesystem
      linuxParameters = {
        capabilities       = { drop = ["ALL"] }
        initProcessEnabled = false
      }
      environment = local.env_list
      secrets     = local.injected_list
      portMappings = var.container_port == null ? [] : [{
        containerPort = var.container_port
        hostPort      = var.container_port
        protocol      = "tcp"
      }]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this.name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = var.service_name
        }
      }
      ulimits     = [{ name = "nofile", softLimit = 65536, hardLimit = 65536 }]
      stopTimeout = var.stop_timeout_seconds
      dependsOn   = var.otel_sidecar_enabled ? [{ containerName = "otel-collector", condition = "START" }] : []
    },
    length(var.command) > 0 ? { command = var.command } : {},
    length(var.container_health_command) > 0 ? {
      healthCheck = {
        command     = var.container_health_command
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 30
      }
    } : {},
  )

  otel_container = {
    name                   = "otel-collector"
    image                  = var.otel_collector_image
    essential              = false
    command                = ["--config=${var.otel_config_path}"]
    readonlyRootFilesystem = true
    linuxParameters        = { capabilities = { drop = ["ALL"] } }
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.this.name
        "awslogs-region"        = var.aws_region
        "awslogs-stream-prefix" = "otel"
      }
    }
  }

  # for-filter instead of a conditional: the two containers have different attribute sets.
  containers = concat([local.app_container], [for c in [local.otel_container] : c if var.otel_sidecar_enabled])

  task_statement_count = (length(var.runtime_secret_arns) > 0 ? 1 : 0) + length(var.task_policy_statements) + (var.otel_sidecar_enabled ? 1 : 0)

  # The primary target group decides whether the task is in service; extras
  # (the liveness group on /v1/healthz) register the same container so their
  # health metric is an independent signal.
  target_group_arns = var.target_group_arn == null ? [] : concat([var.target_group_arn], var.extra_target_group_arns)

  autoscaled          = var.autoscaling_max_capacity != null
  autoscaling_enabled = var.create_service && local.autoscaled
}

resource "aws_cloudwatch_log_group" "this" {
  name              = "/ecs/${var.name_prefix}/${var.service_name}"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.logs_kms_key_arn
  tags              = var.tags
}

# ---------------------------------------------------------------------------
# Roles
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [var.aws_account_id]
    }
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:aws:ecs:${var.aws_region}:${var.aws_account_id}:*"]
    }
  }
}

resource "aws_iam_role" "task" {
  permissions_boundary = var.permissions_boundary_arn
  name                 = "${local.full_name}-task"
  description          = "Runtime identity of ${var.service_name} (SYSTEM.md section 2 credential scope)"
  assume_role_policy   = data.aws_iam_policy_document.assume.json
  tags                 = merge(var.tags, { Binary = var.service_name })
}

resource "aws_iam_role" "execution" {
  permissions_boundary = var.permissions_boundary_arn
  name                 = "${local.full_name}-exec"
  description          = "ECS agent identity for ${var.service_name}: image pull, logs, injected secrets"
  assume_role_policy   = data.aws_iam_policy_document.assume.json
  tags                 = merge(var.tags, { Binary = var.service_name })
}

data "aws_iam_policy_document" "execution" {
  #trivy:ignore:AVD-AWS-0057 ecr:GetAuthorizationToken does not support resource-level permissions
  statement {
    sid       = "ECRAuth"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid       = "ECRPull"
    actions   = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability"]
    resources = local.ecr_repository_arns
  }

  statement {
    sid       = "Logs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.this.arn}:*"]
  }

  dynamic "statement" {
    for_each = length(local.injected_secret_arns) > 0 ? [1] : []
    content {
      sid       = "InjectedSecrets"
      actions   = ["secretsmanager:GetSecretValue"]
      resources = local.injected_secret_arns
    }
  }

  dynamic "statement" {
    for_each = length(local.injected_secret_arns) > 0 ? [1] : []
    content {
      sid       = "InjectedSecretsDecrypt"
      actions   = ["kms:Decrypt"]
      resources = [var.secrets_kms_key_arn]
      condition {
        test     = "StringEquals"
        variable = "kms:ViaService"
        values   = ["secretsmanager.${var.aws_region}.amazonaws.com"]
      }
    }
  }
}

resource "aws_iam_role_policy" "execution" {
  name   = "execution"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.execution.json
}

data "aws_iam_policy_document" "task" {
  dynamic "statement" {
    for_each = length(var.runtime_secret_arns) > 0 ? [1] : []
    content {
      sid       = "ResolveSecretRefs"
      actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
      resources = var.runtime_secret_arns
    }
  }

  dynamic "statement" {
    for_each = length(var.runtime_secret_arns) > 0 ? [1] : []
    content {
      sid       = "DecryptSecretRefs"
      actions   = ["kms:Decrypt"]
      resources = [var.secrets_kms_key_arn]
      condition {
        test     = "StringEquals"
        variable = "kms:ViaService"
        values   = ["secretsmanager.${var.aws_region}.amazonaws.com"]
      }
    }
  }

  dynamic "statement" {
    for_each = var.task_policy_statements
    content {
      sid       = statement.value.sid
      actions   = statement.value.actions
      resources = statement.value.resources
    }
  }

  # ADOT sidecar shares the task role: EMF metrics, X-Ray traces, log streams.
  #trivy:ignore:AVD-AWS-0057 cloudwatch:PutMetricData and xray:Put* do not support resource-level permissions; PutMetricData is namespace-scoped by condition
  dynamic "statement" {
    for_each = var.otel_sidecar_enabled ? [1] : []
    content {
      sid       = "OtelExport"
      actions   = ["cloudwatch:PutMetricData"]
      resources = ["*"]
      condition {
        test     = "StringEquals"
        variable = "cloudwatch:namespace"
        values   = [var.otel_metric_namespace]
      }
    }
  }

  dynamic "statement" {
    for_each = var.otel_sidecar_enabled ? [1] : []
    content {
      sid       = "OtelTracesAndLogs"
      actions   = ["xray:PutTraceSegments", "xray:PutTelemetryRecords", "logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
      resources = ["*"]
    }
  }
}

resource "aws_iam_role_policy" "task" {
  count  = local.task_statement_count > 0 ? 1 : 0
  name   = "task"
  role   = aws_iam_role.task.id
  policy = data.aws_iam_policy_document.task.json
}

# ---------------------------------------------------------------------------
# Task definition and service
# ---------------------------------------------------------------------------

resource "aws_ecs_task_definition" "this" {
  family                   = local.full_name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.cpu)
  memory                   = tostring(var.memory)
  task_role_arn            = aws_iam_role.task.arn
  execution_role_arn       = aws_iam_role.execution.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.cpu_architecture
  }

  ephemeral_storage {
    size_in_gib = var.ephemeral_storage_gib
  }

  container_definitions = jsonencode(local.containers)

  tags = merge(var.tags, { Binary = var.service_name })
}

resource "aws_ecs_service" "this" {
  count = var.create_service && !local.autoscaled ? 1 : 0

  name            = local.full_name
  cluster         = var.cluster_arn
  task_definition = aws_ecs_task_definition.this.arn
  desired_count   = var.desired_count
  launch_type     = "FARGATE"

  enable_execute_command  = false
  enable_ecs_managed_tags = true
  propagate_tags          = "SERVICE"

  deployment_minimum_healthy_percent = var.deployment_minimum_healthy_percent
  deployment_maximum_percent         = var.deployment_maximum_percent
  health_check_grace_period_seconds  = var.target_group_arn == null ? null : var.health_check_grace_period_seconds

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  deployment_controller {
    type = "ECS"
  }

  network_configuration {
    subnets          = var.subnet_ids
    security_groups  = var.security_group_ids
    assign_public_ip = false
  }

  dynamic "load_balancer" {
    for_each = local.target_group_arns
    content {
      target_group_arn = load_balancer.value
      container_name   = var.service_name
      container_port   = var.container_port
    }
  }

  tags = merge(var.tags, { Binary = var.service_name })
}

# Same service, but Application Auto Scaling owns the task count. `lifecycle`
# blocks cannot be made conditional, and `ignore_changes = [desired_count]` is
# required once a scaling policy is attached: without it every `terraform apply`
# would scale the api back down to the floor and let the policy climb out of it
# again, which during a busy period is a self-inflicted capacity dip. Splitting
# the resource is the only way to have that lifecycle in one mode and not the
# other; `var.desired_count` is the initial count here, the floor is
# `autoscaling_min_capacity`.
resource "aws_ecs_service" "scaled" {
  count = var.create_service && local.autoscaled ? 1 : 0

  name            = local.full_name
  cluster         = var.cluster_arn
  task_definition = aws_ecs_task_definition.this.arn
  desired_count   = var.desired_count
  launch_type     = "FARGATE"

  enable_execute_command  = false
  enable_ecs_managed_tags = true
  propagate_tags          = "SERVICE"

  deployment_minimum_healthy_percent = var.deployment_minimum_healthy_percent
  deployment_maximum_percent         = var.deployment_maximum_percent
  health_check_grace_period_seconds  = var.target_group_arn == null ? null : var.health_check_grace_period_seconds

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  deployment_controller {
    type = "ECS"
  }

  network_configuration {
    subnets          = var.subnet_ids
    security_groups  = var.security_group_ids
    assign_public_ip = false
  }

  dynamic "load_balancer" {
    for_each = local.target_group_arns
    content {
      target_group_arn = load_balancer.value
      container_name   = var.service_name
      container_port   = var.container_port
    }
  }

  lifecycle {
    ignore_changes = [desired_count]
  }

  tags = merge(var.tags, { Binary = var.service_name })
}

# ---------------------------------------------------------------------------
# Autoscaling (api only). Two target-tracking policies, no step scaling: the
# api is stateless behind the ALB, so the honest signals are "each task is
# working too hard" and "each task is being asked for too much".
# ---------------------------------------------------------------------------

resource "aws_appautoscaling_target" "this" {
  count = local.autoscaling_enabled ? 1 : 0

  service_namespace  = "ecs"
  resource_id        = "service/${var.cluster_name}/${one(aws_ecs_service.scaled[*].name)}"
  scalable_dimension = "ecs:service:DesiredCount"
  min_capacity       = var.autoscaling_min_capacity
  max_capacity       = var.autoscaling_max_capacity
  tags               = merge(var.tags, { Binary = var.service_name })
}

resource "aws_appautoscaling_policy" "cpu" {
  count = local.autoscaling_enabled ? 1 : 0

  name               = "${local.full_name}-cpu"
  policy_type        = "TargetTrackingScaling"
  service_namespace  = aws_appautoscaling_target.this[0].service_namespace
  resource_id        = aws_appautoscaling_target.this[0].resource_id
  scalable_dimension = aws_appautoscaling_target.this[0].scalable_dimension

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
    target_value = var.autoscaling_cpu_target
    # Scale out fast, scale in slowly: a wrong scale-out costs money, a wrong
    # scale-in costs latency on the money path.
    scale_out_cooldown = var.autoscaling_scale_out_cooldown
    scale_in_cooldown  = var.autoscaling_scale_in_cooldown
  }
}

resource "aws_appautoscaling_policy" "requests" {
  count = local.autoscaling_enabled && var.autoscaling_alb_resource_label != null ? 1 : 0

  name               = "${local.full_name}-requests"
  policy_type        = "TargetTrackingScaling"
  service_namespace  = aws_appautoscaling_target.this[0].service_namespace
  resource_id        = aws_appautoscaling_target.this[0].resource_id
  scalable_dimension = aws_appautoscaling_target.this[0].scalable_dimension

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ALBRequestCountPerTarget"
      resource_label         = var.autoscaling_alb_resource_label
    }
    target_value       = var.autoscaling_requests_per_target
    scale_out_cooldown = var.autoscaling_scale_out_cooldown
    scale_in_cooldown  = var.autoscaling_scale_in_cooldown
  }
}
