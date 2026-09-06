terraform {
  required_version = ">= 1.9, < 2.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0" # 6.63.0 was current on registry.terraform.io when pinned (2026-09-06)
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
