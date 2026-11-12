resource "aws_s3_bucket" "invoices" {
  bucket = "acme-invoices"
  tags = {
    team        = "platform"
    cost_center = "cc-42"
  }
}
