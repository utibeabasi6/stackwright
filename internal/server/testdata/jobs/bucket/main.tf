terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}

# Fake keys: a plan of new resources calls no AWS API.
provider "aws" {
  region                      = "eu-west-1"
  access_key                  = "eval"
  secret_key                  = "eval"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
}

resource "aws_s3_bucket" "logs" {
  bucket = "logs-unprefixed"
}
