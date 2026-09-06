# Secrets Manager (PART 99/100). One entry per aws-sm:// reference in
# .env.example that a deployed binary needs, and the least-privilege matrix
# that decides which task role may read which secret (SYSTEM.md section 2).
# Every task's environment carries the *reference* (aws-sm://<name>); the value
# is resolved at runtime by the SecretRef resolver with the task role, so a
# role without a grant here fails closed the first time the code needs it.

locals {
  # secret name -> service keys allowed to read it.
  matrix = {
    # Database URLs (PART 101). readonly goes only to the audit-worker; api
    # readers fall back to the app URL by design.
    "database/app-url"      = ["api", "relay-worker", "execution-worker", "reconciliation-worker", "market-ingest-worker", "agent-worker", "workflow-worker"]
    "database/migrate-url"  = ["migrate"]
    "database/readonly-url" = ["audit-worker"]

    # Role passwords are consumed by the db-bootstrap one-shot task only.
    "database/roles/cp_migrate-password"  = ["db-bootstrap"]
    "database/roles/cp_app-password"      = ["db-bootstrap"]
    "database/roles/cp_readonly-password" = ["db-bootstrap"]
    "database/roles/cp_ops-password"      = ["db-bootstrap"]

    # Redis: rate limits and caches; not for ingest or audit.
    "redis/url" = ["api", "execution-worker", "reconciliation-worker", "agent-worker", "workflow-worker"]

    # Event bus / analytics credentials. The relay-worker is the outbox's only
    # publisher, so it needs the bus and the app database and nothing else.
    "redpanda/sasl-username" = ["api", "relay-worker", "execution-worker", "reconciliation-worker", "market-ingest-worker", "agent-worker", "workflow-worker"]
    "redpanda/sasl-password" = ["api", "relay-worker", "execution-worker", "reconciliation-worker", "market-ingest-worker", "agent-worker", "workflow-worker"]
    "clickhouse/username"    = ["api", "market-ingest-worker"]
    "clickhouse/password"    = ["api", "market-ingest-worker"]

    # Identity.
    "auth/oidc-client-secret" = ["api"]

    # Providers (PART 100 blast radius).
    "provider/funding/api-key"                 = ["api", "workflow-worker"]
    "provider/funding/webhook-secret"          = ["api"]
    "provider/wallet/api-key"                  = ["execution-worker", "workflow-worker"]
    "provider/wallet/webhook-secret"           = ["api"]
    "provider/signing/api-key"                 = ["execution-worker"]
    "provider/execution/api-key"               = ["execution-worker", "reconciliation-worker"]
    "provider/market-data/api-key"             = ["market-ingest-worker"]
    "provider/chain-observer/api-key"          = ["execution-worker", "reconciliation-worker", "workflow-worker"]
    "provider/chain-observer-fallback/api-key" = ["execution-worker", "reconciliation-worker"]
    "provider/model/api-key"                   = ["agent-worker"]
    "provider/notification/api-key"            = ["api", "workflow-worker"]
  }

  services = distinct(flatten(values(local.matrix)))

  # Resolved principals per secret (unknown service keys are dropped: fail closed).
  readers = {
    for name, svcs in local.matrix :
    name => compact([for s in svcs : lookup(var.task_role_arns, s, "")])
  }

  secrets_of = {
    for svc in local.services :
    svc => [for name, svcs in local.matrix : name if contains(svcs, svc)]
  }
}

# The matrix is data; these assertions are the contract (SYSTEM.md section 2).
check "least_privilege_matrix" {
  assert {
    condition     = tolist(local.matrix["provider/signing/api-key"]) == tolist(["execution-worker"])
    error_message = "Only the execution-worker may hold the signing provider credential."
  }
  assert {
    condition     = !contains(local.matrix["provider/signing/api-key"], "api") && !contains(local.matrix["provider/wallet/api-key"], "api")
    error_message = "The api must never hold wallet or signing credentials."
  }
  assert {
    condition = length(setsubtract(toset(local.secrets_of["agent-worker"]), toset([
      "database/app-url", "redis/url", "redpanda/sasl-username", "redpanda/sasl-password", "provider/model/api-key",
    ]))) == 0
    error_message = "The agent-worker may hold only DB app, Redis, bus and model credentials: no wallet, signing, funding, execution or admin secrets."
  }
  assert {
    condition = length(setsubtract(toset(local.secrets_of["market-ingest-worker"]), toset([
      "database/app-url", "redpanda/sasl-username", "redpanda/sasl-password", "clickhouse/username", "clickhouse/password", "provider/market-data/api-key",
    ]))) == 0
    error_message = "The market-ingest-worker may hold only data-provider, bus, ClickHouse and ingest-state DB credentials."
  }
  assert {
    condition     = tolist(local.secrets_of["migrate"]) == tolist(["database/migrate-url"])
    error_message = "The migrate task holds the migration URL and nothing else."
  }
  assert {
    condition = length(setsubtract(toset(local.secrets_of["relay-worker"]), toset([
      "database/app-url", "redpanda/sasl-username", "redpanda/sasl-password",
    ]))) == 0
    error_message = "The relay-worker drains the outbox to the bus: app DB and Redpanda only, never Redis, ClickHouse, KMS or any provider credential."
  }
  assert {
    condition     = !contains(local.matrix["provider/signing/api-key"], "reconciliation-worker") && !contains(local.matrix["provider/signing/api-key"], "workflow-worker")
    error_message = "Reconciliation and workflow workers never hold signing credentials."
  }
}

resource "aws_secretsmanager_secret" "this" {
  for_each                = local.matrix
  name                    = "${var.secret_name_prefix}/${each.key}"
  description             = "${var.secret_name_prefix} ${each.key}; readers: ${join(", ", each.value)}"
  kms_key_id              = var.kms_key_arn
  recovery_window_in_days = var.recovery_window_in_days
  tags                    = merge(var.tags, { Readers = join(" ", each.value) })
}

# Values Terraform owns (currently redis/url). Operators populate the rest.
resource "aws_secretsmanager_secret_version" "managed" {
  for_each      = toset([for k in var.managed_secret_keys : k if contains(keys(local.matrix), k)])
  secret_id     = aws_secretsmanager_secret.this[each.key].id
  secret_string = var.managed_secret_values[each.key]
}

data "aws_iam_policy_document" "secret" {
  for_each = local.matrix

  dynamic "statement" {
    for_each = length(local.readers[each.key]) > 0 ? [1] : []
    content {
      sid       = "AllowTaskRoles"
      actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
      resources = ["*"]
      principals {
        type        = "AWS"
        identifiers = local.readers[each.key]
      }
    }
  }

  # Resource-level enforcement: even an over-broad identity policy cannot read
  # this secret unless the principal is a listed reader or an operator.
  statement {
    sid       = "DenyEveryoneElse"
    effect    = "Deny"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = ["*"]
    principals {
      type        = "AWS"
      identifiers = ["*"]
    }
    condition {
      test     = "ArnNotLike"
      variable = "aws:PrincipalArn"
      values   = concat(local.readers[each.key], var.admin_principal_arns)
    }
  }
}

resource "aws_secretsmanager_secret_policy" "this" {
  for_each   = local.matrix
  secret_arn = aws_secretsmanager_secret.this[each.key].arn
  policy     = data.aws_iam_policy_document.secret[each.key].json
}

resource "aws_secretsmanager_secret_rotation" "this" {
  for_each            = { for k, v in var.rotation : k => v if contains(keys(local.matrix), k) }
  secret_id           = aws_secretsmanager_secret.this[each.key].id
  rotation_lambda_arn = each.value.lambda_arn
  rotation_rules {
    automatically_after_days = each.value.automatically_after_days
  }
}
