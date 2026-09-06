# Remote state: S3 with the native S3 lock file (Terraform >= 1.10) and KMS.
# Terraform forbids variables inside backend blocks, so the bucket, key and
# region come from a partial configuration file passed at init time:
#
#   terraform init -backend-config=backend.hcl
#
# backend.hcl.example documents the expected keys. The state bucket and its
# KMS key are bootstrapped by hand once per account (DEPLOYMENT.md section 2)
# because Terraform cannot create the bucket that stores its own state.
# A DynamoDB table is no longer required for locking; if your state bucket
# predates use_lockfile, add dynamodb_table to backend.hcl during migration.

terraform {
  backend "s3" {
    encrypt      = true
    use_lockfile = true
  }
}
