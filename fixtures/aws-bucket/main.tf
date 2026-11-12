# Creates one S3 bucket with the credentials a run gets from its cloud
# account. Used to check AWS by hand (docs/status.md, phase 7).
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

resource "aws_s3_bucket" "check" {
  bucket_prefix = "stackwright-check-"
  force_destroy = true
}

output "bucket" {
  value = aws_s3_bucket.check.bucket
}
