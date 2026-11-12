resource "aws_cloudwatch_log_group" "api" {
  name              = "/app/api"
  retention_in_days = 14
}
