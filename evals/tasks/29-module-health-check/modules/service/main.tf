resource "aws_lb_target_group" "this" {
  name     = var.name
  port     = var.port
  protocol = "HTTP"
  vpc_id   = var.vpc_id

  health_check {
    path     = var.health_check.path
    interval = var.health_check.interval_seconds
  }
}

resource "aws_cloudwatch_log_group" "this" {
  name              = "/service/${var.name}"
  retention_in_days = 30
}
