output "deploy_role_arn" {
  description = "Set as the AWS_RELEASE_ROLE_ARN repository variable for release.yml."
  value       = aws_iam_role.deploy.arn
}

output "oidc_provider_arn" {
  value = local.oidc_provider_arn
}
